package plugins

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"

	"github.com/SafwanGanz/Sonicos/internal/command"
)

const (
	maxStickerStrikes  = 3
	floodStickerLimit  = 4
	spamMessageLimit   = 6
	floodWindowSeconds = 10
	shieldWarnCooldown = 10
)

var groupLinkRe = regexp.MustCompile(`(?i)\b(https?://|www\.|chat\.whatsapp\.com/|wa\.me/|t\.me/|discord\.gg/)`)

func init() {
	command.Watch(watchBannedStickers)

	for _, cmd := range []command.Command{
		{
			Name:        "bansticker",
			Aliases:     []string{"blsticker", "blocksticker"},
			Category:    "Group (Admins)",
			Description: "Blacklist a replied sticker or sound sticker in this group",
			Handler:     banStickerCmd,
		},
		{
			Name:        "unbansticker",
			Aliases:     []string{"unblsticker", "allowsticker"},
			Category:    "Group (Admins)",
			Description: "Remove a sticker from this group's blacklist (reply or hash)",
			Handler:     unbanStickerCmd,
		},
		{
			Name:        "bannedstickers",
			Aliases:     []string{"blstickers", "stickerlist"},
			Category:    "Group (Admins)",
			Description: "List blacklisted stickers in this group",
			Handler:     listBannedStickersCmd,
		},
		{
			Name:        "warnings",
			Aliases:     []string{"warns", "strikes"},
			Category:    "Group (Admins)",
			Description: "Check a member's sticker warning count",
			Handler:     warningsCmd,
		},
		{
			Name:        "resetwarns",
			Aliases:     []string{"clearwarns", "unwarn"},
			Category:    "Group (Admins)",
			Description: "Reset a member's sticker warnings to 0",
			Handler:     resetWarningsCmd,
		},
		{
			Name:        "antiloud",
			Aliases:     []string{"antiearrape", "loudguard"},
			Category:    "Group (Admins)",
			Description: "Auto-guard against extremely loud audio & audio stickers: on, off, or dB",
			Handler:     antiLoudCmd,
		},
		{
			Name:        "antilink",
			Category:    "Group (Admins)",
			Description: "Toggle automatic link blocking for non-admins: on or off",
			Handler:     antiLinkCmd,
		},
		{
			Name:        "antispam",
			Category:    "Group (Admins)",
			Description: "Toggle message flood protection for non-admins: on or off",
			Handler:     antiSpamCmd,
		},
		{
			Name:        "antistickerflood",
			Aliases:     []string{"antiflood"},
			Category:    "Group (Admins)",
			Description: "Toggle sticker flood protection for non-admins: on or off",
			Handler:     antiStickerFloodCmd,
		},
		{
			Name:        "shields",
			Aliases:     []string{"antistatus"},
			Category:    "Group (Admins)",
			Description: "Show active group moderation shields and thresholds",
			Handler:     shieldsStatusCmd,
		},
	} {
		command.Register(cmd)
	}
}

func watchBannedStickers(ctx context.Context, msg command.Message, reply command.ReplyFunc) error {
	if !msg.Group || msg.FromMe || msg.ModerationStore == nil {
		return nil
	}

	if msg.Sticker != nil {

		if err := msg.ModerationStore.RememberSticker(ctx, msg.Chat, msg.ID, *msg.Sticker); err != nil {
			slog.Warn("cache incoming sticker metadata", "chat", msg.Chat, "message_id", msg.ID, "error", err)
		}

		entry, banned, err := msg.ModerationStore.IsStickerBanned(ctx, msg.Chat, msg.Sticker.FileSHA256, msg.Sticker.AudioSHA256)
		if err != nil {
			return err
		}
		if banned {
			return handleBlacklistedStickerMatch(ctx, msg, entry, reply)
		}
	}

	shields, err := msg.ModerationStore.GetShields(ctx, msg.Chat)
	if err != nil {
		slog.Warn("load group shields", "chat", msg.Chat, "error", err)
		return nil
	}
	if !shields.AntiLoud && !shields.AntiLink && !shields.AntiSpam && !shields.AntiStickerFlood {
		return nil
	}
	if msg.GroupService == nil {
		return nil
	}
	info, err := msg.GroupService.GetInfo(ctx, msg.Chat)
	if err != nil {
		slog.Error("fetch group info for shields", "chat", msg.Chat, "error", err)
		return nil
	}

	userID := jidUser(msg.Sender)
	isAdmin := isParticipantAdmin(info, msg.Sender)
	botIsAdmin := info.BotJID != "" && isParticipantAdmin(info, info.BotJID)

	if shields.AntiLoud {
		if triggered, err := checkAntiLoud(ctx, msg, shields, userID, isAdmin, botIsAdmin, reply); err != nil || triggered {
			return err
		}
	}

	if isAdmin {
		return nil
	}

	if shields.AntiLink && msg.Text != "" && containsLink(msg.Text) {
		if botIsAdmin {
			return enforceMemberViolation(ctx, msg, userID, "sent a blocked link", reply)
		}
		if w, _ := msg.ModerationStore.IncrementRate(ctx, msg.Chat, "link_warn", userID, shieldWarnCooldown); w == 1 {
			return reply(ctx, command.Reply{
				Text:     fmt.Sprintf("⚠️ @%s, links are not allowed in this group.", userID),
				Mentions: []string{msg.Sender},
			})
		}
		return nil
	}

	if shields.AntiStickerFlood && msg.Sticker != nil {
		count, err := msg.ModerationStore.IncrementRate(ctx, msg.Chat, "stkflood", userID, floodWindowSeconds)
		if err == nil && count > floodStickerLimit {
			if botIsAdmin {
				if err := msg.GroupService.RevokeMessage(ctx, msg.Chat, msg.Sender, msg.ID); err != nil {
					slog.Warn("revoke sticker flood", "chat", msg.Chat, "message_id", msg.ID, "error", err)
				}
			}
			if w, _ := msg.ModerationStore.IncrementRate(ctx, msg.Chat, "stkflood_warn", userID, shieldWarnCooldown); w == 1 {
				if botIsAdmin {
					return enforceStrikeOnly(ctx, msg, userID, "flooded stickers", reply)
				}
				return reply(ctx, command.Reply{
					Text:     fmt.Sprintf("⚠️ @%s, please stop flooding stickers.", userID),
					Mentions: []string{msg.Sender},
				})
			}
			return nil
		}
	}

	if shields.AntiSpam {
		count, err := msg.ModerationStore.IncrementRate(ctx, msg.Chat, "msgspam", userID, floodWindowSeconds)
		if err == nil && count > spamMessageLimit {
			if botIsAdmin {
				if err := msg.GroupService.RevokeMessage(ctx, msg.Chat, msg.Sender, msg.ID); err != nil {
					slog.Warn("revoke spam message", "chat", msg.Chat, "message_id", msg.ID, "error", err)
				}
			}
			if w, _ := msg.ModerationStore.IncrementRate(ctx, msg.Chat, "msgspam_warn", userID, shieldWarnCooldown); w == 1 {
				if botIsAdmin {
					return enforceStrikeOnly(ctx, msg, userID, "spammed messages", reply)
				}
				return reply(ctx, command.Reply{
					Text:     fmt.Sprintf("⚠️ @%s, please slow down and stop spamming.", userID),
					Mentions: []string{msg.Sender},
				})
			}
		}
	}

	return nil
}

func handleBlacklistedStickerMatch(ctx context.Context, msg command.Message, entry command.BannedSticker, reply command.ReplyFunc) error {
	if msg.GroupService == nil {
		return nil
	}
	info, err := msg.GroupService.GetInfo(ctx, msg.Chat)
	if err != nil {
		slog.Error("fetch group info for sticker moderation", "chat", msg.Chat, "error", err)
		return nil
	}

	userID := jidUser(msg.Sender)
	botIsAdmin := info.BotJID != "" && isParticipantAdmin(info, info.BotJID)
	reasonSuffix := ""
	if strings.TrimSpace(entry.Reason) != "" {
		reasonSuffix = fmt.Sprintf(" Reason: %s.", strings.TrimSpace(entry.Reason))
	}

	if isParticipantAdmin(info, msg.Sender) {
		if botIsAdmin {
			if err := msg.GroupService.RevokeMessage(ctx, msg.Chat, msg.Sender, msg.ID); err != nil {
				slog.Error("revoke blacklisted sticker from admin", "chat", msg.Chat, "message_id", msg.ID, "error", err)
			}
			warnText := fmt.Sprintf("⚠️ @%s, that sticker is blacklisted in this group and was deleted.%s", userID, reasonSuffix)
			return reply(ctx, command.Reply{Text: warnText, Mentions: []string{msg.Sender}})
		}
		warnText := fmt.Sprintf("⚠️ @%s, that sticker is blacklisted in this group.%s", userID, reasonSuffix)
		return reply(ctx, command.Reply{Text: warnText, Mentions: []string{msg.Sender}})
	}

	if !botIsAdmin {
		warnText := fmt.Sprintf("⚠️ NSFW / 18+ sticker from @%s — watch at your own risk!%s", userID, reasonSuffix)
		if msg.Sticker.IsAudioSticker || entry.AudioSHA256 != "" {
			warnText = fmt.Sprintf("⚠️ NSFW / 18+ audio sticker from @%s — watch/listen at your own risk!%s", userID, reasonSuffix)
		}
		return reply(ctx, command.Reply{Text: warnText, Mentions: []string{msg.Sender}})
	}

	strikes, err := msg.ModerationStore.AddStrike(ctx, msg.Chat, userID)
	if err != nil {
		return err
	}

	if err := msg.GroupService.RevokeMessage(ctx, msg.Chat, msg.Sender, msg.ID); err != nil {
		slog.Error("revoke blacklisted sticker", "chat", msg.Chat, "message_id", msg.ID, "error", err)
	}
	if strikes >= maxStickerStrikes {
		warnText := fmt.Sprintf(
			"@%s sent a blacklisted sticker and reached %d/%d warnings — removing from the group.%s",
			userID, strikes, maxStickerStrikes, reasonSuffix,
		)
		if err := reply(ctx, command.Reply{Text: warnText, Mentions: []string{msg.Sender}}); err != nil {
			return err
		}
		if err := msg.GroupService.UpdateParticipants(ctx, msg.Chat, []string{msg.Sender}, command.ParticipantRemove); err != nil {
			slog.Error("kick user after sticker strikes", "chat", msg.Chat, "user", msg.Sender, "error", err)
			return reply(ctx, command.Reply{Text: "Could not remove the user automatically; please remove them manually."})
		}
		if err := msg.ModerationStore.ResetStrikes(ctx, msg.Chat, userID); err != nil {
			slog.Warn("reset strikes after kick", "chat", msg.Chat, "user", userID, "error", err)
		}
		return nil
	}

	warnText := fmt.Sprintf(
		"⚠️ @%s, that sticker is blacklisted in this group and was deleted. Warning %d/%d (auto-removal at %d).%s",
		userID, strikes, maxStickerStrikes, maxStickerStrikes, reasonSuffix,
	)
	return reply(ctx, command.Reply{Text: warnText, Mentions: []string{msg.Sender}})
}

func checkAntiLoud(
	ctx context.Context,
	msg command.Message,
	shields command.GroupShields,
	userID string,
	isAdmin, botIsAdmin bool,
	reply command.ReplyFunc,
) (bool, error) {
	src := msg.StickerAudioSource
	if src == nil && msg.Sticker == nil && msg.MediaSource != nil && msg.Text == "" {
		src = msg.MediaSource
	}
	if src == nil {
		return false, nil
	}
	att, err := src.Download(ctx)
	if err != nil || att.Kind != command.MediaAudio {
		return false, nil
	}
	meanDB, maxDB, err := stickerConverter.MeasureLoudness(ctx, att.Data)
	if err != nil {
		slog.Debug("measure loudness skipped", "chat", msg.Chat, "error", err)
		return false, nil
	}
	if meanDB < shields.AntiLoudDB || maxDB < -2.0 {
		return false, nil
	}

	if isAdmin {
		if botIsAdmin {
			if err := msg.GroupService.RevokeMessage(ctx, msg.Chat, msg.Sender, msg.ID); err != nil {
				slog.Warn("revoke loud audio from admin", "chat", msg.Chat, "error", err)
			}
			return true, reply(ctx, command.Reply{
				Text:     fmt.Sprintf("⚠️ @%s, that audio is extremely loud (%.1f dB) and was deleted.", userID, meanDB),
				Mentions: []string{msg.Sender},
			})
		}
		return true, reply(ctx, command.Reply{
			Text:     fmt.Sprintf("⚠️ @%s, that audio is extremely loud (%.1f dB).", userID, meanDB),
			Mentions: []string{msg.Sender},
		})
	}

	if !botIsAdmin {
		return true, reply(ctx, command.Reply{
			Text:     fmt.Sprintf("⚠️ Extremely loud / earrape audio from @%s (%.1f dB) — listen at your own risk!", userID, meanDB),
			Mentions: []string{msg.Sender},
		})
	}
	return true, enforceMemberViolation(ctx, msg, userID, fmt.Sprintf("sent extremely loud audio (%.1f dB)", meanDB), reply)
}

func enforceMemberViolation(ctx context.Context, msg command.Message, userID, reason string, reply command.ReplyFunc) error {
	if err := msg.GroupService.RevokeMessage(ctx, msg.Chat, msg.Sender, msg.ID); err != nil {
		slog.Warn("revoke violating message", "chat", msg.Chat, "message_id", msg.ID, "error", err)
	}
	return enforceStrikeOnly(ctx, msg, userID, reason, reply)
}

func enforceStrikeOnly(ctx context.Context, msg command.Message, userID, reason string, reply command.ReplyFunc) error {
	strikes, err := msg.ModerationStore.AddStrike(ctx, msg.Chat, userID)
	if err != nil {
		return err
	}
	if strikes >= maxStickerStrikes {
		warnText := fmt.Sprintf(
			"@%s %s and reached %d/%d warnings — removing from the group.",
			userID, reason, strikes, maxStickerStrikes,
		)
		if err := reply(ctx, command.Reply{Text: warnText, Mentions: []string{msg.Sender}}); err != nil {
			return err
		}
		if err := msg.GroupService.UpdateParticipants(ctx, msg.Chat, []string{msg.Sender}, command.ParticipantRemove); err != nil {
			slog.Error("kick user after shield strikes", "chat", msg.Chat, "user", msg.Sender, "error", err)
			return reply(ctx, command.Reply{Text: "Could not remove the user automatically; please remove them manually."})
		}
		_ = msg.ModerationStore.ResetStrikes(ctx, msg.Chat, userID)
		return nil
	}
	warnText := fmt.Sprintf(
		"⚠️ @%s %s and the message was deleted. Warning %d/%d (auto-removal at %d).",
		userID, reason, strikes, maxStickerStrikes, maxStickerStrikes,
	)
	return reply(ctx, command.Reply{Text: warnText, Mentions: []string{msg.Sender}})
}

func containsLink(text string) bool {
	return groupLinkRe.MatchString(text)
}

func resolveQuotedStickerInfo(ctx context.Context, msg command.Message) command.StickerInfo {
	var out command.StickerInfo
	if msg.QuotedSticker != nil {
		out = *msg.QuotedSticker
	}
	if msg.ModerationStore != nil && msg.QuotedMessageID != "" {
		if cached, ok, err := msg.ModerationStore.LookupSticker(ctx, msg.Chat, msg.QuotedMessageID); err == nil && ok {
			if out.FileSHA256 == "" {
				out.FileSHA256 = cached.FileSHA256
			}
			if out.AudioSHA256 == "" {
				out.AudioSHA256 = cached.AudioSHA256
			}
			if !out.IsAudioSticker {
				out.IsAudioSticker = cached.IsAudioSticker
			}
			if !out.IsAnimated {
				out.IsAnimated = cached.IsAnimated
			}
		}
	}
	return out
}

func shortHash(h string) string {
	h = strings.TrimSpace(h)
	if len(h) > 8 {
		return h[:8]
	}
	return h
}

func banStickerCmd(ctx context.Context, msg command.Message, args []string, reply command.ReplyFunc) error {
	info, ok, err := requireGroupAdmin(ctx, msg, reply, false)
	if !ok || err != nil {
		return err
	}
	if msg.ModerationStore == nil {
		return reply(ctx, command.Reply{Text: "Sticker moderation storage is unavailable right now."})
	}

	st := resolveQuotedStickerInfo(ctx, msg)
	if st.FileSHA256 == "" && st.AudioSHA256 == "" {
		return reply(ctx, command.Reply{
			Text: fmt.Sprintf("Reply to the sticker or sound sticker you want to blacklist, then send %sbansticker [reason].", botPrefix()),
		})
	}

	reason := strings.TrimSpace(strings.Join(args, " "))
	entry := command.BannedSticker{
		FileSHA256:  st.FileSHA256,
		AudioSHA256: st.AudioSHA256,
		Reason:      reason,
		AddedBy:     jidUser(msg.Sender),
	}
	if err := msg.ModerationStore.BanSticker(ctx, msg.Chat, entry); err != nil {
		slog.Error("ban sticker", "chat", msg.Chat, "error", err)
		return reply(ctx, command.Reply{Text: "Couldn't blacklist that sticker. Try again."})
	}

	deleted := false
	if info.BotJID != "" && isParticipantAdmin(info, info.BotJID) && msg.QuotedMessageID != "" {
		if err := msg.GroupService.RevokeMessage(ctx, msg.Chat, msg.QuotedSender, msg.QuotedMessageID); err != nil {
			slog.Warn("revoke banned quoted sticker", "chat", msg.Chat, "message_id", msg.QuotedMessageID, "error", err)
		} else {
			deleted = true
		}
	}

	kindLabel := "Sticker"
	if st.IsAudioSticker || st.AudioSHA256 != "" {
		kindLabel = "Audio sticker"
	}
	idLabel := shortHash(st.FileSHA256)
	if st.AudioSHA256 != "" {
		if idLabel != "" {
			idLabel += " / audio:" + shortHash(st.AudioSHA256)
		} else {
			idLabel = "audio:" + shortHash(st.AudioSHA256)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "🚫 %s blacklisted (`%s`).", kindLabel, idLabel)
	if deleted {
		b.WriteString(" Quoted sticker deleted.")
	}
	if reason != "" {
		fmt.Fprintf(&b, "\nReason: %s", reason)
	}
	return reply(ctx, command.Reply{Text: b.String()})
}

func unbanStickerCmd(ctx context.Context, msg command.Message, args []string, reply command.ReplyFunc) error {
	if _, ok, err := requireGroupAdmin(ctx, msg, reply, false); !ok || err != nil {
		return err
	}
	if msg.ModerationStore == nil {
		return reply(ctx, command.Reply{Text: "Sticker moderation storage is unavailable right now."})
	}

	if msg.QuotedSticker != nil || msg.QuotedMessageID != "" {
		st := resolveQuotedStickerInfo(ctx, msg)
		if st.FileSHA256 != "" || st.AudioSHA256 != "" {
			removed, err := msg.ModerationStore.UnbanSticker(ctx, msg.Chat, st.FileSHA256, st.AudioSHA256)
			if err != nil {
				slog.Error("unban quoted sticker", "chat", msg.Chat, "error", err)
				return reply(ctx, command.Reply{Text: "Couldn't remove that sticker from the blacklist."})
			}
			if !removed {
				return reply(ctx, command.Reply{Text: "That sticker is not on this group's blacklist."})
			}
			return reply(ctx, command.Reply{Text: "✅ Sticker removed from this group's blacklist."})
		}
	}

	if len(args) > 0 {
		removed, err := msg.ModerationStore.UnbanStickerByPrefix(ctx, msg.Chat, args[0])
		if err != nil {
			return reply(ctx, command.Reply{Text: err.Error()})
		}
		if !removed {
			return reply(ctx, command.Reply{Text: "No blacklisted sticker matched that hash prefix."})
		}
		return reply(ctx, command.Reply{Text: "✅ Sticker removed from this group's blacklist."})
	}

	return reply(ctx, command.Reply{
		Text: fmt.Sprintf("Reply to a sticker or pass its hash prefix from %sbannedstickers.", botPrefix()),
	})
}

func listBannedStickersCmd(ctx context.Context, msg command.Message, _ []string, reply command.ReplyFunc) error {
	if _, ok, err := requireGroupAdmin(ctx, msg, reply, false); !ok || err != nil {
		return err
	}
	if msg.ModerationStore == nil {
		return reply(ctx, command.Reply{Text: "Sticker moderation storage is unavailable right now."})
	}

	entries, err := msg.ModerationStore.ListBannedStickers(ctx, msg.Chat)
	if err != nil {
		slog.Error("list banned stickers", "chat", msg.Chat, "error", err)
		return reply(ctx, command.Reply{Text: "Couldn't load blacklisted stickers."})
	}
	if len(entries) == 0 {
		return reply(ctx, command.Reply{Text: "No stickers are blacklisted in this group."})
	}

	var b strings.Builder
	fmt.Fprintf(&b, "🚫 *Blacklisted Stickers (%d)*\n", len(entries))
	for i, e := range entries {
		id := shortHash(e.FileSHA256)
		if id == "" {
			id = shortHash(e.AudioSHA256)
		}
		audioNote := ""
		if e.AudioSHA256 != "" {
			audioNote = " [audio:" + shortHash(e.AudioSHA256) + "]"
		}
		reason := ""
		if e.Reason != "" {
			reason = " — " + e.Reason
		}
		fmt.Fprintf(&b, "%d. `%s`%s%s\n", i+1, id, audioNote, reason)
	}
	fmt.Fprintf(&b, "\nUse `%sunbansticker <hash>` to remove an entry.", botPrefix())
	return reply(ctx, command.Reply{Text: b.String()})
}

func warningsCmd(ctx context.Context, msg command.Message, args []string, reply command.ReplyFunc) error {
	if _, ok, err := requireGroupAdmin(ctx, msg, reply, false); !ok || err != nil {
		return err
	}
	if msg.ModerationStore == nil {
		return reply(ctx, command.Reply{Text: "Sticker moderation storage is unavailable right now."})
	}

	targets := resolveTargets(msg, args)
	if len(targets) == 0 {
		targets = []string{msg.Sender}
	}
	var lines []string
	var mentions []string
	for _, t := range targets {
		uid := jidUser(t)
		count, err := msg.ModerationStore.GetStrikes(ctx, msg.Chat, uid)
		if err != nil {
			slog.Error("get warnings", "chat", msg.Chat, "user", uid, "error", err)
			return reply(ctx, command.Reply{Text: "Couldn't load warnings right now."})
		}
		lines = append(lines, fmt.Sprintf("- @%s: %d/%d", uid, count, maxStickerStrikes))
		if strings.Contains(t, "@") {
			mentions = append(mentions, t)
		}
	}
	return reply(ctx, command.Reply{
		Text:     "Sticker Warnings:\n" + strings.Join(lines, "\n"),
		Mentions: mentions,
	})
}

func resetWarningsCmd(ctx context.Context, msg command.Message, args []string, reply command.ReplyFunc) error {
	if _, ok, err := requireGroupAdmin(ctx, msg, reply, false); !ok || err != nil {
		return err
	}
	if msg.ModerationStore == nil {
		return reply(ctx, command.Reply{Text: "Sticker moderation storage is unavailable right now."})
	}

	targets := resolveTargets(msg, args)
	if len(targets) == 0 {
		return reply(ctx, command.Reply{
			Text: fmt.Sprintf("Mention, reply to, or pass the phone number of the member(s) for %sresetwarns.", botPrefix()),
		})
	}
	for _, t := range targets {
		if err := msg.ModerationStore.ResetStrikes(ctx, msg.Chat, jidUser(t)); err != nil {
			slog.Error("reset warnings", "chat", msg.Chat, "user", t, "error", err)
			return reply(ctx, command.Reply{Text: "Couldn't reset warnings right now."})
		}
	}
	return reply(ctx, command.Reply{Text: fmt.Sprintf("Reset sticker warnings for %d member(s).", len(targets))})
}

func antiLoudCmd(ctx context.Context, msg command.Message, args []string, reply command.ReplyFunc) error {
	if _, ok, err := requireGroupAdmin(ctx, msg, reply, false); !ok || err != nil {
		return err
	}
	if msg.ModerationStore == nil {
		return reply(ctx, command.Reply{Text: "Moderation storage is unavailable right now."})
	}
	if len(args) == 0 {
		return reply(ctx, command.Reply{Text: fmt.Sprintf("Usage: %santiloud <on|off|dB> (e.g. -6)", botPrefix())})
	}

	arg := strings.ToLower(strings.TrimSpace(args[0]))
	switch arg {
	case "on", "enable", "true":
		if err := msg.ModerationStore.SetShield(ctx, msg.Chat, "anti_loud", "1"); err != nil {
			return reply(ctx, command.Reply{Text: "Couldn't update anti-loud setting."})
		}
		shields, _ := msg.ModerationStore.GetShields(ctx, msg.Chat)
		return reply(ctx, command.Reply{Text: fmt.Sprintf("🔊 Anti-earrape loudness guard turned ON (threshold: %.1f dB).", shields.AntiLoudDB)})
	case "off", "disable", "false":
		if err := msg.ModerationStore.SetShield(ctx, msg.Chat, "anti_loud", "0"); err != nil {
			return reply(ctx, command.Reply{Text: "Couldn't update anti-loud setting."})
		}
		return reply(ctx, command.Reply{Text: "🔇 Anti-earrape loudness guard turned OFF."})
	default:
		db, err := strconv.ParseFloat(strings.TrimSuffix(arg, "db"), 64)
		if err != nil || db > 0 || db < -30 {
			return reply(ctx, command.Reply{Text: fmt.Sprintf("Usage: %santiloud <on|off|-20..-1>", botPrefix())})
		}
		if err := msg.ModerationStore.SetShield(ctx, msg.Chat, "anti_loud_db", fmt.Sprintf("%.1f", db)); err != nil {
			return reply(ctx, command.Reply{Text: "Couldn't update anti-loud threshold."})
		}
		if err := msg.ModerationStore.SetShield(ctx, msg.Chat, "anti_loud", "1"); err != nil {
			return reply(ctx, command.Reply{Text: "Couldn't enable anti-loud guard."})
		}
		return reply(ctx, command.Reply{Text: fmt.Sprintf("🔊 Anti-earrape loudness guard ON (threshold: %.1f dB).", db)})
	}
}

func antiLinkCmd(ctx context.Context, msg command.Message, args []string, reply command.ReplyFunc) error {
	return toggleShieldCmd(ctx, msg, args, reply, "anti_link", "antilink", "Anti-Link shield")
}

func antiSpamCmd(ctx context.Context, msg command.Message, args []string, reply command.ReplyFunc) error {
	return toggleShieldCmd(ctx, msg, args, reply, "anti_spam", "antispam", "Anti-Spam shield")
}

func antiStickerFloodCmd(ctx context.Context, msg command.Message, args []string, reply command.ReplyFunc) error {
	return toggleShieldCmd(ctx, msg, args, reply, "anti_sticker_flood", "antistickerflood", "Anti-Sticker-Flood shield")
}

func toggleShieldCmd(ctx context.Context, msg command.Message, args []string, reply command.ReplyFunc, field, cmdName, label string) error {
	if _, ok, err := requireGroupAdmin(ctx, msg, reply, false); !ok || err != nil {
		return err
	}
	if msg.ModerationStore == nil {
		return reply(ctx, command.Reply{Text: "Moderation storage is unavailable right now."})
	}
	if len(args) == 0 {
		return reply(ctx, command.Reply{Text: fmt.Sprintf("Usage: %s%s <on|off>", botPrefix(), cmdName)})
	}
	var val string
	var state string
	switch strings.ToLower(strings.TrimSpace(args[0])) {
	case "on", "enable", "true":
		val = "1"
		state = "ON"
	case "off", "disable", "false":
		val = "0"
		state = "OFF"
	default:
		return reply(ctx, command.Reply{Text: fmt.Sprintf("Usage: %s%s <on|off>", botPrefix(), cmdName)})
	}
	if err := msg.ModerationStore.SetShield(ctx, msg.Chat, field, val); err != nil {
		slog.Error("set group shield", "field", field, "chat", msg.Chat, "error", err)
		return reply(ctx, command.Reply{Text: "Couldn't update shield setting."})
	}
	return reply(ctx, command.Reply{Text: fmt.Sprintf("🛡️ %s turned %s.", label, state)})
}

func shieldsStatusCmd(ctx context.Context, msg command.Message, _ []string, reply command.ReplyFunc) error {
	if _, ok, err := requireGroupAdmin(ctx, msg, reply, false); !ok || err != nil {
		return err
	}
	if msg.ModerationStore == nil {
		return reply(ctx, command.Reply{Text: "Moderation storage is unavailable right now."})
	}
	s, err := msg.ModerationStore.GetShields(ctx, msg.Chat)
	if err != nil {
		return reply(ctx, command.Reply{Text: "Couldn't load group shield settings."})
	}
	return reply(ctx, command.Reply{
		Text: fmt.Sprintf(
			"🛡️ *Group Shields*\n• Anti-Loud (earrape): *%s* (%.1f dB)\n• Anti-Link: *%s*\n• Anti-Spam (>%d msgs/%ds): *%s*\n• Anti-Sticker-Flood (>%d stickers/%ds): *%s*",
			boolLabel(s.AntiLoud, "ON", "OFF"), s.AntiLoudDB,
			boolLabel(s.AntiLink, "ON", "OFF"),
			spamMessageLimit, floodWindowSeconds, boolLabel(s.AntiSpam, "ON", "OFF"),
			floodStickerLimit, floodWindowSeconds, boolLabel(s.AntiStickerFlood, "ON", "OFF"),
		),
	})
}
