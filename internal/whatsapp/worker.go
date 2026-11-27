package whatsapp

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/SafwanGanz/Sonicos/internal/command"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types/events"
)

func (b *Bot) runWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt := <-b.queue:
			if evt == nil || evt.Message == nil {
				continue
			}
			if err := b.processMessage(ctx, evt); err != nil {
				b.logger.Error("command processing failed", "chat", evt.Info.Chat.String(), "message_id", evt.Info.ID, "error", err)
			}
		}
	}
}

func (b *Bot) rememberSoundSticker(ctx context.Context, chatJID, messageID string, msg *waE2E.Message) {
	if b.audioCache == nil || msg == nil {
		return
	}
	b.audioCache.Remember(ctx, b.moderation, chatJID, messageID, msg)
}

var quotedAudioCommands = map[string]struct{}{
	"steal":        {},
	"take":         {},
	"rebrand":      {},
	"extract":      {},
	"toaudio":      {},
	"mp3":          {},
	"tovideo":      {},
	"mp4":          {},
	"dub":          {},
	"audiosticker": {},
	"stdio":        {},
	"sticker":      {},
	"s":            {},
	"sreact":       {},
	"sr":           {},
	"bansticker":   {},
	"blsticker":    {},
	"blocksticker": {},
	"unbansticker": {},
	"unblsticker":  {},
	"allowsticker": {},
	"mix":          {},
	"merge":        {},
	"voice":        {},
	"sound":        {},
}

func maybeNeedsQuotedStickerAudio(text string) bool {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 0 {
		return false
	}
	first := fields[0]
	r, _ := utf8.DecodeRuneInString(first)
	if unicode.IsLetter(r) || unicode.IsDigit(r) {
		return false
	}
	cmdWord := strings.TrimFunc(strings.ToLower(first), func(ch rune) bool {
		return !unicode.IsLetter(ch) && !unicode.IsDigit(ch)
	})
	_, ok := quotedAudioCommands[cmdWord]
	return ok
}

func (b *Bot) hydrateQuotedSoundSticker(ctx context.Context, msg *command.Message) {
	if b.audioCache == nil || msg == nil || msg.StickerAudioSource != nil {
		return
	}
	if msg.QuotedMessageID == "" && msg.QuotedSticker == nil {
		return
	}
	var quotedSHA string
	if msg.QuotedSticker != nil {
		quotedSHA = msg.QuotedSticker.FileSHA256
	}
	cachedAudio := b.audioCache.LookupMemory(msg.Chat, msg.QuotedMessageID, quotedSHA)
	if cachedAudio == nil && (msg.QuotedSticker != nil || maybeNeedsQuotedStickerAudio(msg.Text)) {
		cachedAudio = b.audioCache.Lookup(ctx, b.moderation, msg.Chat, msg.QuotedMessageID, quotedSHA)
	}
	if cachedAudio == nil {
		return
	}
	src := newAudioMediaSource(b.client, cachedAudio)
	if src == nil {
		return
	}
	msg.StickerAudioSource = src
	if msg.QuotedSticker != nil {
		msg.QuotedSticker.IsAudioSticker = true
		if msg.QuotedSticker.AudioSHA256 == "" && len(cachedAudio.GetFileSHA256()) > 0 {
			msg.QuotedSticker.AudioSHA256 = hex.EncodeToString(cachedAudio.GetFileSHA256())
		}
	}
}

func (b *Bot) processMessage(ctx context.Context, evt *events.Message) error {
	start := time.Now()
	b.rememberSoundSticker(ctx, evt.Info.Chat.String(), string(evt.Info.ID), evt.Message)

	message, ok := DecodeMessage(b.client, evt)
	if !ok {
		return nil
	}
	message.ModerationStore = b.moderation
	if b.groups != nil {
		message.GroupService = b.groups
	}
	b.hydrateQuotedSoundSticker(ctx, &message)

	reply := func(ctx context.Context, response command.Reply) error {
		var outgoing *waE2E.Message
		var err error
		switch {
		case response.StickerReaction != nil:
			outgoing, err = b.encodeStickerReaction(ctx, *response.StickerReaction, evt.Info.Chat)
		case response.Video != nil:
			outgoing, err = b.encodeVideoReply(ctx, *response.Video, evt)
		case response.VideoNote != nil:
			outgoing, err = b.encodeVideoNoteReply(ctx, *response.VideoNote, evt)
		case response.Image != nil:
			outgoing, err = b.encodeImageReply(ctx, *response.Image, evt)
		case response.Audio != nil:
			outgoing, err = b.encodeAudioReply(ctx, *response.Audio, evt)
		case response.Sticker != nil:
			outgoing, err = b.encodeSticker(ctx, *response.Sticker, evt)
		default:
			if response.Text == "" {
				return fmt.Errorf("reply must contain text or media")
			}
			outgoing = EncodeQuotedReply(evt, response.Text, response.Mentions...)
		}
		if err != nil {
			return err
		}

		// Try sending with quoted reply first, fallback to non-quoted on sender key errors
		sendWithFallback := func(msg *waE2E.Message) error {
			sendStart := time.Now()
			resp, err := b.client.SendMessage(ctx, evt.Info.Chat, msg)
			sendElapsed := time.Since(sendStart)
			totalElapsed := time.Since(start)
			if err != nil {
				// Check if it's a sender key error - retry without quoting
				errStr := err.Error()
				if strings.Contains(errStr, "sender key") || strings.Contains(errStr, "senderKey") {
					// Retry without quoted context for any message type
					fallbackMsg := StripQuotedContext(msg)
					if b.logger != nil {
						b.logger.Warn("sender key error, retrying without quote", "chat", evt.Info.Chat.String(), "error", err)
					}
					resp, err = b.client.SendMessage(ctx, evt.Info.Chat, fallbackMsg)
					sendElapsed = time.Since(sendStart)
					totalElapsed = time.Since(start)
					msg = fallbackMsg // Use fallback msg for logging/remember
				}
				if err != nil {
					return fmt.Errorf("send quoted reply: %w", err)
				}
			}
			if b.logger != nil {
				attrs := []any{
					"chat", evt.Info.Chat.String(),
					"message_id", string(evt.Info.ID),
					"total_ms", totalElapsed.Milliseconds(),
					"send_ms", sendElapsed.Milliseconds(),
					"wa_queue_ms", resp.DebugTimings.Queue.Milliseconds(),
					"wa_get_devices_ms", resp.DebugTimings.GetDevices.Milliseconds(),
					"wa_group_encrypt_ms", resp.DebugTimings.GroupEncrypt.Milliseconds(),
					"wa_peer_encrypt_ms", resp.DebugTimings.PeerEncrypt.Milliseconds(),
					"wa_send_ms", resp.DebugTimings.Send.Milliseconds(),
					"wa_resp_ms", resp.DebugTimings.Resp.Milliseconds(),
					"wa_retry_ms", resp.DebugTimings.Retry.Milliseconds(),
				}
				if totalElapsed >= 500*time.Millisecond {
					b.logger.Info("slow WhatsApp reply timing", attrs...)
				} else {
					b.logger.Debug("WhatsApp reply timing", attrs...)
				}
			}
			b.rememberSoundSticker(ctx, evt.Info.Chat.String(), string(resp.ID), msg)
			return nil
		}

		return sendWithFallback(outgoing)
	}
	return b.processor.Handle(ctx, message, reply)
}
