package plugins

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/SafwanGanz/Sonicos/internal/command"
)

func init() {
	for _, cmd := range []command.Command{

		{Name: "groupinfo", Aliases: []string{"ginfo", "gi"}, Category: "Group (Members)", Description: "Show group metadata and settings", Handler: groupInfo},
		{Name: "admins", Aliases: []string{"staff"}, Category: "Group (Members)", Description: "List and mention all group admins", Handler: listAdmins},

		{Name: "add", Category: "Group (Admins)", Description: "Add member(s) by phone number", Handler: addMembers},
		{Name: "addmode", Category: "Group (Admins)", Description: "Set who can add members: admins or all", Handler: setMemberAddMode},
		{Name: "approval", Category: "Group (Admins)", Description: "Toggle join approval mode: on or off", Handler: setJoinApproval},
		{Name: "approve", Category: "Group (Admins)", Description: "Approve pending join request(s): all, @mention, or number", Handler: approveJoinRequests},
		{Name: "del", Aliases: []string{"delete"}, Category: "Group (Admins)", Description: "Delete the quoted message", Handler: deleteQuotedMessage},
		{Name: "demote", Category: "Group (Admins)", Description: "Demote admin(s) to member", Handler: demoteMembers},
		{Name: "ephemeral", Aliases: []string{"disappear"}, Category: "Group (Admins)", Description: "Set disappearing messages: off, 24h, 7d, or 90d", Handler: setEphemeral},
		{Name: "hidetag", Aliases: []string{"ht"}, Category: "Group (Admins)", Description: "Send a message mentioning all members without listing tags", Handler: hideTag},
		{Name: "kick", Aliases: []string{"remove", "ban"}, Category: "Group (Admins)", Description: "Remove mentioned, quoted, or numbered member(s)", Handler: kickMembers},
		{Name: "leave", Category: "Group (Admins)", Description: "Make the bot leave this group", Handler: leaveGroup},
		{Name: "link", Aliases: []string{"invite"}, Category: "Group (Admins)", Description: "Get the group invite link", Handler: getInviteLink},
		{Name: "lock", Category: "Group (Admins)", Description: "Restrict editing group info to admins only", Handler: lockGroup},
		{Name: "mute", Category: "Group (Admins)", Description: "Restrict group messaging to admins only", Handler: muteGroup},
		{Name: "promote", Category: "Group (Admins)", Description: "Promote member(s) to group admin", Handler: promoteMembers},
		{Name: "reject", Category: "Group (Admins)", Description: "Reject pending join request(s): all, @mention, or number", Handler: rejectJoinRequests},
		{Name: "requests", Category: "Group (Admins)", Description: "List pending group join requests", Handler: listJoinRequests},
		{Name: "revokelink", Aliases: []string{"resetlink"}, Category: "Group (Admins)", Description: "Reset and show a new group invite link", Handler: resetInviteLink},
		{Name: "setdesc", Category: "Group (Admins)", Description: "Change the group description", Handler: setGroupDesc},
		{Name: "setname", Category: "Group (Admins)", Description: "Change the group name", Handler: setGroupName},
		{Name: "setpp", Category: "Group (Admins)", Description: "Set the group photo from an attached or quoted image", Handler: setGroupPhoto},
		{Name: "tagall", Aliases: []string{"everyone"}, Category: "Group (Admins)", Description: "Mention all group members", Handler: tagAll},
		{Name: "unlock", Category: "Group (Admins)", Description: "Allow all members to edit group info", Handler: unlockGroup},
		{Name: "unmute", Category: "Group (Admins)", Description: "Allow all members to send messages", Handler: unmuteGroup},
		{Name: "welcome", Category: "Group (Admins)", Description: "Toggle welcome messages: on or off", Handler: setWelcome},
	} {
		command.Register(cmd)
	}
}

func requireGroup(ctx context.Context, msg command.Message, reply command.ReplyFunc) (command.GroupInfo, bool, error) {
	if !msg.Group {
		return command.GroupInfo{}, false, reply(ctx, command.Reply{Text: "This command can only be used in a group."})
	}
	if msg.GroupService == nil {
		return command.GroupInfo{}, false, reply(ctx, command.Reply{Text: "Group management is unavailable right now."})
	}
	info, err := msg.GroupService.GetInfo(ctx, msg.Chat)
	if err != nil {
		slog.Error("fetch group info", "chat", msg.Chat, "error", err)
		return command.GroupInfo{}, false, reply(ctx, command.Reply{Text: "Couldn't load group info. Try again."})
	}
	return info, true, nil
}

func requireGroupAdmin(ctx context.Context, msg command.Message, reply command.ReplyFunc, requireBotAdmin bool) (command.GroupInfo, bool, error) {
	info, ok, err := requireGroup(ctx, msg, reply)
	if !ok || err != nil {
		return command.GroupInfo{}, false, err
	}
	if !isParticipantAdmin(info, msg.Sender) {
		return command.GroupInfo{}, false, reply(ctx, command.Reply{Text: "Only group admins can use this command."})
	}
	if requireBotAdmin && info.BotJID != "" && !isParticipantAdmin(info, info.BotJID) {
		return command.GroupInfo{}, false, reply(ctx, command.Reply{Text: "I need to be a group admin to do that."})
	}
	return info, true, nil
}

func isParticipantAdmin(info command.GroupInfo, jid string) bool {
	user := jidUser(jid)
	if user == "" {
		return false
	}
	for _, p := range info.Participants {
		if jidUser(p.JID) == user || (p.PhoneNumber != "" && jidUser(p.PhoneNumber) == user) {
			return p.IsAdmin || p.IsSuperAdmin
		}
	}
	return false
}

func jidUser(jid string) string {
	jid = strings.TrimSpace(jid)
	if idx := strings.Index(jid, "@"); idx >= 0 {
		jid = jid[:idx]
	}
	if idx := strings.Index(jid, ":"); idx >= 0 {
		jid = jid[:idx]
	}
	return jid
}

func resolveTargets(msg command.Message, args []string) []string {
	seen := make(map[string]struct{})
	var targets []string
	add := func(t string) {
		t = strings.TrimSpace(t)
		if t == "" {
			return
		}
		key := jidUser(t)
		if key == "" {
			key = t
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		targets = append(targets, t)
	}
	for _, jid := range msg.MentionedJIDs {
		add(jid)
	}
	if len(targets) == 0 && msg.QuotedSender != "" {
		add(msg.QuotedSender)
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "@") {
			continue
		}
		add(arg)
	}
	return targets
}

func groupInfo(ctx context.Context, msg command.Message, _ []string, reply command.ReplyFunc) error {
	info, ok, err := requireGroup(ctx, msg, reply)
	if !ok || err != nil {
		return err
	}
	admins := 0
	for _, p := range info.Participants {
		if p.IsAdmin || p.IsSuperAdmin {
			admins++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "*%s*\n", info.Name)
	fmt.Fprintf(&b, "Members: %d (%d admins)\n", len(info.Participants), admins)
	if info.Owner != "" {
		fmt.Fprintf(&b, "Owner: @%s\n", jidUser(info.Owner))
	}
	fmt.Fprintf(&b, "Messages: %s\n", boolLabel(info.Announce, "admins only", "everyone"))
	fmt.Fprintf(&b, "Edit info: %s\n", boolLabel(info.Locked, "admins only", "everyone"))
	fmt.Fprintf(&b, "Join approval: %s\n", boolLabel(info.Approval, "on", "off"))
	fmt.Fprintf(&b, "Add members: %s\n", boolLabel(info.MemberAddAll, "everyone", "admins only"))
	fmt.Fprintf(&b, "Disappearing: %s", formatEphemeral(info.EphemeralSec))
	if topic := strings.TrimSpace(info.Topic); topic != "" {
		fmt.Fprintf(&b, "\n\nDescription:\n%s", topic)
	}
	var mentions []string
	if info.Owner != "" {
		mentions = append(mentions, info.Owner)
	}
	return reply(ctx, command.Reply{Text: b.String(), Mentions: mentions})
}

func listAdmins(ctx context.Context, msg command.Message, _ []string, reply command.ReplyFunc) error {
	info, ok, err := requireGroup(ctx, msg, reply)
	if !ok || err != nil {
		return err
	}
	var lines []string
	var mentions []string
	for _, p := range info.Participants {
		if !p.IsAdmin && !p.IsSuperAdmin {
			continue
		}
		role := "Admin"
		if p.IsSuperAdmin {
			role = "Owner"
		}
		lines = append(lines, fmt.Sprintf("- @%s (%s)", jidUser(p.JID), role))
		mentions = append(mentions, p.JID)
	}
	if len(lines) == 0 {
		return reply(ctx, command.Reply{Text: "No admins found in this group."})
	}
	return reply(ctx, command.Reply{
		Text:     "Group Admins:\n" + strings.Join(lines, "\n"),
		Mentions: mentions,
	})
}

func tagAll(ctx context.Context, msg command.Message, args []string, reply command.ReplyFunc) error {
	info, ok, err := requireGroupAdmin(ctx, msg, reply, false)
	if !ok || err != nil {
		return err
	}
	var b strings.Builder
	if note := strings.TrimSpace(strings.Join(args, " ")); note != "" {
		b.WriteString(note)
		b.WriteString("\n\n")
	}
	mentions := make([]string, 0, len(info.Participants))
	for i, p := range info.Participants {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "@%s", jidUser(p.JID))
		mentions = append(mentions, p.JID)
	}
	return reply(ctx, command.Reply{Text: b.String(), Mentions: mentions})
}

func hideTag(ctx context.Context, msg command.Message, args []string, reply command.ReplyFunc) error {
	info, ok, err := requireGroupAdmin(ctx, msg, reply, false)
	if !ok || err != nil {
		return err
	}
	text := strings.TrimSpace(strings.Join(args, " "))
	if text == "" {
		text = "Attention everyone."
	}
	mentions := make([]string, 0, len(info.Participants))
	for _, p := range info.Participants {
		mentions = append(mentions, p.JID)
	}
	return reply(ctx, command.Reply{Text: text, Mentions: mentions})
}

func addMembers(ctx context.Context, msg command.Message, args []string, reply command.ReplyFunc) error {
	return handleParticipantAction(ctx, msg, args, reply, command.ParticipantAdd, "Usage: !add <phone_number...>", "Added %d member(s).")
}

func kickMembers(ctx context.Context, msg command.Message, args []string, reply command.ReplyFunc) error {
	return handleParticipantAction(ctx, msg, args, reply, command.ParticipantRemove, "Mention, reply to, or pass the phone number of the member(s) to remove.", "Removed %d member(s).")
}

func promoteMembers(ctx context.Context, msg command.Message, args []string, reply command.ReplyFunc) error {
	return handleParticipantAction(ctx, msg, args, reply, command.ParticipantPromote, "Mention, reply to, or pass the phone number of the member(s) to promote.", "Promoted %d member(s) to admin.")
}

func demoteMembers(ctx context.Context, msg command.Message, args []string, reply command.ReplyFunc) error {
	return handleParticipantAction(ctx, msg, args, reply, command.ParticipantDemote, "Mention, reply to, or pass the phone number of the admin(s) to demote.", "Demoted %d admin(s).")
}

func handleParticipantAction(ctx context.Context, msg command.Message, args []string, reply command.ReplyFunc, action command.ParticipantAction, usage, successFmt string) error {
	info, ok, err := requireGroupAdmin(ctx, msg, reply, true)
	if !ok || err != nil {
		return err
	}
	targets := resolveTargets(msg, args)
	if len(targets) == 0 {
		return reply(ctx, command.Reply{Text: usage})
	}
	if action == command.ParticipantRemove || action == command.ParticipantDemote {
		for _, t := range targets {
			if info.Owner != "" && jidUser(t) == jidUser(info.Owner) {
				return reply(ctx, command.Reply{Text: "Cannot remove or demote the group owner."})
			}
			if info.BotJID != "" && jidUser(t) == jidUser(info.BotJID) && action == command.ParticipantRemove {
				return reply(ctx, command.Reply{Text: "Use !leave to remove me from the group."})
			}
		}
	}
	if err := msg.GroupService.UpdateParticipants(ctx, msg.Chat, targets, action); err != nil {
		slog.Error("update group participants", "action", action, "chat", msg.Chat, "error", err)
		return reply(ctx, command.Reply{Text: fmt.Sprintf("Failed to %s member(s): %v", action, err)})
	}
	return reply(ctx, command.Reply{Text: fmt.Sprintf(successFmt, len(targets))})
}

func muteGroup(ctx context.Context, msg command.Message, _ []string, reply command.ReplyFunc) error {
	return setAnnounce(ctx, msg, reply, true)
}

func unmuteGroup(ctx context.Context, msg command.Message, _ []string, reply command.ReplyFunc) error {
	return setAnnounce(ctx, msg, reply, false)
}

func setAnnounce(ctx context.Context, msg command.Message, reply command.ReplyFunc, announce bool) error {
	if _, ok, err := requireGroupAdmin(ctx, msg, reply, true); !ok || err != nil {
		return err
	}
	if err := msg.GroupService.SetAnnounce(ctx, msg.Chat, announce); err != nil {
		slog.Error("set group announce", "chat", msg.Chat, "error", err)
		return reply(ctx, command.Reply{Text: "Couldn't update group messaging settings."})
	}
	if announce {
		return reply(ctx, command.Reply{Text: "Group muted: only admins can send messages now."})
	}
	return reply(ctx, command.Reply{Text: "Group unmuted: all members can send messages now."})
}

func lockGroup(ctx context.Context, msg command.Message, _ []string, reply command.ReplyFunc) error {
	return setLocked(ctx, msg, reply, true)
}

func unlockGroup(ctx context.Context, msg command.Message, _ []string, reply command.ReplyFunc) error {
	return setLocked(ctx, msg, reply, false)
}

func setLocked(ctx context.Context, msg command.Message, reply command.ReplyFunc, locked bool) error {
	if _, ok, err := requireGroupAdmin(ctx, msg, reply, true); !ok || err != nil {
		return err
	}
	if err := msg.GroupService.SetLocked(ctx, msg.Chat, locked); err != nil {
		slog.Error("set group locked", "chat", msg.Chat, "error", err)
		return reply(ctx, command.Reply{Text: "Couldn't update group info lock settings."})
	}
	if locked {
		return reply(ctx, command.Reply{Text: "Group locked: only admins can edit group info."})
	}
	return reply(ctx, command.Reply{Text: "Group unlocked: all members can edit group info."})
}

func setGroupName(ctx context.Context, msg command.Message, args []string, reply command.ReplyFunc) error {
	if _, ok, err := requireGroupAdmin(ctx, msg, reply, true); !ok || err != nil {
		return err
	}
	name := strings.TrimSpace(strings.Join(args, " "))
	if name == "" {
		return reply(ctx, command.Reply{Text: "Usage: !setname <new group name>"})
	}
	if err := msg.GroupService.SetName(ctx, msg.Chat, name); err != nil {
		slog.Error("set group name", "chat", msg.Chat, "error", err)
		return reply(ctx, command.Reply{Text: "Couldn't update the group name."})
	}
	return reply(ctx, command.Reply{Text: fmt.Sprintf("Group name updated to %q.", name)})
}

func setGroupDesc(ctx context.Context, msg command.Message, args []string, reply command.ReplyFunc) error {
	if _, ok, err := requireGroupAdmin(ctx, msg, reply, true); !ok || err != nil {
		return err
	}
	desc := strings.TrimSpace(strings.Join(args, " "))
	if desc == "" {
		return reply(ctx, command.Reply{Text: "Usage: !setdesc <new description>"})
	}
	if err := msg.GroupService.SetDescription(ctx, msg.Chat, desc); err != nil {
		slog.Error("set group description", "chat", msg.Chat, "error", err)
		return reply(ctx, command.Reply{Text: "Couldn't update the group description."})
	}
	return reply(ctx, command.Reply{Text: "Group description updated."})
}

func setGroupPhoto(ctx context.Context, msg command.Message, _ []string, reply command.ReplyFunc) error {
	if _, ok, err := requireGroupAdmin(ctx, msg, reply, true); !ok || err != nil {
		return err
	}
	if msg.MediaSource == nil {
		return reply(ctx, command.Reply{Text: "Attach or reply to an image, then send !setpp."})
	}
	media, err := msg.MediaSource.Download(ctx)
	if err != nil {
		slog.Error("download group photo", "chat", msg.Chat, "error", err)
		return reply(ctx, command.Reply{Text: "Couldn't download the image."})
	}
	if media.Kind != command.MediaImage {
		return reply(ctx, command.Reply{Text: "Group photo must be an image."})
	}
	if err := msg.GroupService.SetPhoto(ctx, msg.Chat, media.Data); err != nil {
		slog.Error("set group photo", "chat", msg.Chat, "error", err)
		return reply(ctx, command.Reply{Text: "Couldn't update the group photo."})
	}
	return reply(ctx, command.Reply{Text: "Group photo updated."})
}

func getInviteLink(ctx context.Context, msg command.Message, _ []string, reply command.ReplyFunc) error {
	return inviteLink(ctx, msg, reply, false)
}

func resetInviteLink(ctx context.Context, msg command.Message, _ []string, reply command.ReplyFunc) error {
	return inviteLink(ctx, msg, reply, true)
}

func inviteLink(ctx context.Context, msg command.Message, reply command.ReplyFunc, reset bool) error {
	if _, ok, err := requireGroupAdmin(ctx, msg, reply, true); !ok || err != nil {
		return err
	}
	link, err := msg.GroupService.GetInviteLink(ctx, msg.Chat, reset)
	if err != nil {
		slog.Error("get invite link", "chat", msg.Chat, "reset", reset, "error", err)
		return reply(ctx, command.Reply{Text: "Couldn't fetch the group invite link."})
	}
	if reset {
		return reply(ctx, command.Reply{Text: "Invite link reset:\n" + link})
	}
	return reply(ctx, command.Reply{Text: "Group invite link:\n" + link})
}

func setJoinApproval(ctx context.Context, msg command.Message, args []string, reply command.ReplyFunc) error {
	if _, ok, err := requireGroupAdmin(ctx, msg, reply, true); !ok || err != nil {
		return err
	}
	if len(args) == 0 {
		return reply(ctx, command.Reply{Text: "Usage: !approval <on|off>"})
	}
	var mode bool
	switch strings.ToLower(args[0]) {
	case "on", "true", "enable":
		mode = true
	case "off", "false", "disable":
		mode = false
	default:
		return reply(ctx, command.Reply{Text: "Usage: !approval <on|off>"})
	}
	if err := msg.GroupService.SetJoinApproval(ctx, msg.Chat, mode); err != nil {
		slog.Error("set join approval", "chat", msg.Chat, "error", err)
		return reply(ctx, command.Reply{Text: "Couldn't update join approval mode."})
	}
	return reply(ctx, command.Reply{Text: fmt.Sprintf("Join approval mode turned %s.", boolLabel(mode, "on", "off"))})
}

func listJoinRequests(ctx context.Context, msg command.Message, _ []string, reply command.ReplyFunc) error {
	if _, ok, err := requireGroupAdmin(ctx, msg, reply, true); !ok || err != nil {
		return err
	}
	reqs, err := msg.GroupService.GetJoinRequests(ctx, msg.Chat)
	if err != nil {
		slog.Error("get join requests", "chat", msg.Chat, "error", err)
		return reply(ctx, command.Reply{Text: "Couldn't load pending join requests."})
	}
	if len(reqs) == 0 {
		return reply(ctx, command.Reply{Text: "No pending join requests."})
	}
	var lines []string
	var mentions []string
	for _, r := range reqs {
		lines = append(lines, "- @"+jidUser(r.JID))
		mentions = append(mentions, r.JID)
	}
	return reply(ctx, command.Reply{
		Text:     fmt.Sprintf("Pending join requests (%d):\n%s", len(reqs), strings.Join(lines, "\n")),
		Mentions: mentions,
	})
}

func approveJoinRequests(ctx context.Context, msg command.Message, args []string, reply command.ReplyFunc) error {
	return handleJoinRequests(ctx, msg, args, reply, true)
}

func rejectJoinRequests(ctx context.Context, msg command.Message, args []string, reply command.ReplyFunc) error {
	return handleJoinRequests(ctx, msg, args, reply, false)
}

func handleJoinRequests(ctx context.Context, msg command.Message, args []string, reply command.ReplyFunc, approve bool) error {
	if _, ok, err := requireGroupAdmin(ctx, msg, reply, true); !ok || err != nil {
		return err
	}
	var targets []string
	if len(args) > 0 && strings.EqualFold(args[0], "all") {
		reqs, err := msg.GroupService.GetJoinRequests(ctx, msg.Chat)
		if err != nil {
			return reply(ctx, command.Reply{Text: "Couldn't load pending join requests."})
		}
		for _, r := range reqs {
			targets = append(targets, r.JID)
		}
		if len(targets) == 0 {
			return reply(ctx, command.Reply{Text: "No pending join requests."})
		}
	} else {
		targets = resolveTargets(msg, args)
	}
	if len(targets) == 0 {
		action := "reject"
		if approve {
			action = "approve"
		}
		return reply(ctx, command.Reply{Text: fmt.Sprintf("Usage: !%s <all|@user|phone_number>", action)})
	}
	if err := msg.GroupService.UpdateJoinRequests(ctx, msg.Chat, targets, approve); err != nil {
		slog.Error("update join requests", "approve", approve, "chat", msg.Chat, "error", err)
		return reply(ctx, command.Reply{Text: "Couldn't update join requests."})
	}
	if approve {
		return reply(ctx, command.Reply{Text: fmt.Sprintf("Approved %d join request(s).", len(targets))})
	}
	return reply(ctx, command.Reply{Text: fmt.Sprintf("Rejected %d join request(s).", len(targets))})
}

func setMemberAddMode(ctx context.Context, msg command.Message, args []string, reply command.ReplyFunc) error {
	if _, ok, err := requireGroupAdmin(ctx, msg, reply, true); !ok || err != nil {
		return err
	}
	if len(args) == 0 {
		return reply(ctx, command.Reply{Text: "Usage: !addmode <admins|all>"})
	}
	var allowAll bool
	switch strings.ToLower(args[0]) {
	case "all", "everyone", "members":
		allowAll = true
	case "admins", "admin":
		allowAll = false
	default:
		return reply(ctx, command.Reply{Text: "Usage: !addmode <admins|all>"})
	}
	if err := msg.GroupService.SetMemberAddMode(ctx, msg.Chat, allowAll); err != nil {
		slog.Error("set member add mode", "chat", msg.Chat, "error", err)
		return reply(ctx, command.Reply{Text: "Couldn't update member add mode."})
	}
	return reply(ctx, command.Reply{Text: fmt.Sprintf("Who can add members: %s.", boolLabel(allowAll, "everyone", "admins only"))})
}

func setEphemeral(ctx context.Context, msg command.Message, args []string, reply command.ReplyFunc) error {
	if _, ok, err := requireGroupAdmin(ctx, msg, reply, true); !ok || err != nil {
		return err
	}
	if len(args) == 0 {
		return reply(ctx, command.Reply{Text: "Usage: !ephemeral <off|24h|7d|90d>"})
	}
	var seconds uint32
	switch strings.ToLower(args[0]) {
	case "off", "0":
		seconds = 0
	case "24h", "1d":
		seconds = 86400
	case "7d", "1w":
		seconds = 7 * 86400
	case "90d", "3m":
		seconds = 90 * 86400
	default:
		return reply(ctx, command.Reply{Text: "Supported durations: off, 24h, 7d, 90d."})
	}
	if err := msg.GroupService.SetDisappearing(ctx, msg.Chat, seconds); err != nil {
		slog.Error("set disappearing timer", "chat", msg.Chat, "error", err)
		return reply(ctx, command.Reply{Text: "Couldn't update disappearing messages timer."})
	}
	return reply(ctx, command.Reply{Text: "Disappearing messages set to " + formatEphemeral(seconds) + "."})
}

func deleteQuotedMessage(ctx context.Context, msg command.Message, _ []string, reply command.ReplyFunc) error {
	if _, ok, err := requireGroupAdmin(ctx, msg, reply, true); !ok || err != nil {
		return err
	}
	if msg.QuotedMessageID == "" {
		return reply(ctx, command.Reply{Text: "Reply to the message you want to delete, then send !del."})
	}
	if err := msg.GroupService.RevokeMessage(ctx, msg.Chat, msg.QuotedSender, msg.QuotedMessageID); err != nil {
		slog.Error("revoke message", "chat", msg.Chat, "target", msg.QuotedMessageID, "error", err)
		return reply(ctx, command.Reply{Text: "Couldn't delete that message."})
	}
	return nil
}

func leaveGroup(ctx context.Context, msg command.Message, _ []string, reply command.ReplyFunc) error {
	if _, ok, err := requireGroupAdmin(ctx, msg, reply, false); !ok || err != nil {
		return err
	}
	if err := reply(ctx, command.Reply{Text: "Leaving the group. Goodbye!"}); err != nil {
		return err
	}
	return msg.GroupService.Leave(ctx, msg.Chat)
}

func boolLabel(v bool, trueLabel, falseLabel string) string {
	if v {
		return trueLabel
	}
	return falseLabel
}

func formatEphemeral(seconds uint32) string {
	switch seconds {
	case 0:
		return "off"
	case 86400:
		return "24 hours"
	case 7 * 86400:
		return "7 days"
	case 90 * 86400:
		return "90 days"
	default:
		return fmt.Sprintf("%d seconds", seconds)
	}
}

func setWelcome(ctx context.Context, msg command.Message, args []string, reply command.ReplyFunc) error {
	if _, ok, err := requireGroupAdmin(ctx, msg, reply, false); !ok || err != nil {
		return err
	}
	if len(args) == 0 {
		return reply(ctx, command.Reply{Text: "Usage: !welcom <on|off>"})
	}
	var enabled bool
	switch strings.ToLower(args[0]) {
	case "on", "true", "enable":
		enabled = true
	case "off", "false", "disable":
		enabled = false
	default:
		return reply(ctx, command.Reply{Text: "Usage: !welcom <on|off>"})
	}
	if msg.ModerationStore == nil {
		return reply(ctx, command.Reply{Text: "Welcome settings storage is unavailable."})
	}
	if err := msg.ModerationStore.SetWelcomeEnabled(ctx, msg.Chat, enabled); err != nil {
		slog.Error("set welcome enabled", "chat", msg.Chat, "error", err)
		return reply(ctx, command.Reply{Text: "Couldn't update welcome setting."})
	}
	return reply(ctx, command.Reply{Text: fmt.Sprintf("Welcome messages turned %s.", boolLabel(enabled, "on", "off"))})
}
