package whatsapp

import (
	"encoding/hex"
	"strings"

	"github.com/SafwanGanz/Sonicos/internal/command"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func ShouldIgnoreMessage(evt *events.Message) bool {
	return evt == nil ||
		evt.Info.Chat.IsBroadcastList() ||
		evt.SourceWebMsg != nil ||
		evt.UnavailableRequestID != ""
}

func DecodeMessage(client *whatsmeow.Client, evt *events.Message) (command.Message, bool) {
	if ShouldIgnoreMessage(evt) || evt.Message == nil {
		return command.Message{}, false
	}
	text := extractText(evt.Message)
	stickerInfo := extractStickerInfo(evt.Message)
	if text == "" && stickerInfo == nil && !evt.Info.IsGroup {
		return command.Message{}, false
	}
	ctxInfo := messageContextInfo(evt.Message)
	var mentioned []string
	var quotedSender, quotedID string
	if ctxInfo != nil {
		mentioned = append([]string(nil), ctxInfo.GetMentionedJID()...)
		quotedSender = ctxInfo.GetParticipant()
		quotedID = ctxInfo.GetStanzaID()
	}
	var quotedFromMe bool
	if client != nil && client.Store != nil && client.Store.ID != nil && quotedSender != "" {
		quotedFromMe = stripJIDUser(quotedSender) == client.Store.ID.User
	}
	return command.Message{
		ID:                 string(evt.Info.ID),
		Chat:               evt.Info.Chat.String(),
		Sender:             evt.Info.Sender.ToNonAD().String(),
		FromMe:             evt.Info.IsFromMe,
		Text:               text,
		Group:              evt.Info.IsGroup,
		MentionedJIDs:      mentioned,
		QuotedSender:       quotedSender,
		QuotedMessageID:    quotedID,
		QuotedFromMe:       quotedFromMe,
		Sticker:            stickerInfo,
		QuotedSticker:      extractStickerInfo(quotedMessage(evt.Message)),
		MediaSource:        newMediaSource(client, evt.Message),
		StickerAudioSource: newStickerAudioSource(client, evt.Message),
		GroupService:       newGroupService(client),
	}, true
}

func stripJIDUser(jid string) string {
	jid = strings.TrimSpace(jid)
	if idx := strings.Index(jid, "@"); idx >= 0 {
		jid = jid[:idx]
	}
	if idx := strings.Index(jid, ":"); idx >= 0 {
		jid = jid[:idx]
	}
	return jid
}

func EncodeQuotedReply(evt *events.Message, text string, mentions ...string) *waE2E.Message {
	ctxInfo := quotedContext(evt)
	if len(mentions) > 0 {
		ctxInfo.MentionedJID = mentions
	}
	return &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text:        proto.String(text),
			ContextInfo: ctxInfo,
		},
	}
}

func quotedMessage(message *waE2E.Message) *waE2E.Message {
	if info := messageContextInfo(message); info != nil {
		return info.GetQuotedMessage()
	}
	return nil
}

func messageContextInfo(message *waE2E.Message) *waE2E.ContextInfo {
	message = unwrapMessage(message, 0)
	if message == nil {
		return nil
	}
	for _, info := range []*waE2E.ContextInfo{
		message.GetExtendedTextMessage().GetContextInfo(),
		message.GetImageMessage().GetContextInfo(),
		message.GetVideoMessage().GetContextInfo(),
		message.GetPtvMessage().GetContextInfo(),
		message.GetAudioMessage().GetContextInfo(),
		message.GetDocumentMessage().GetContextInfo(),
	} {
		if info != nil {
			return info
		}
	}
	return nil
}

func extractText(message *waE2E.Message) string {
	message = unwrapMessage(message, 0)
	if message == nil {
		return ""
	}
	for _, text := range []string{
		message.GetConversation(),
		message.GetExtendedTextMessage().GetText(),
		message.GetImageMessage().GetCaption(),
		message.GetVideoMessage().GetCaption(),
		message.GetPtvMessage().GetCaption(),
		message.GetDocumentMessage().GetCaption(),
	} {
		if text = strings.TrimSpace(text); text != "" {
			return text
		}
	}
	return ""
}

func extractStickerInfo(message *waE2E.Message) *command.StickerInfo {
	st, viaAudioWrapper := unwrapStickerMessage(message, false, 0)
	if st == nil {
		return nil
	}
	fileSHA := hex.EncodeToString(st.GetFileSHA256())
	var audioSHA string
	hasAudio := viaAudioWrapper
	if au := st.GetAudioMessage(); au != nil {
		hasAudio = true
		audioSHA = hex.EncodeToString(au.GetFileSHA256())
	}
	if fileSHA == "" && audioSHA == "" {
		return nil
	}
	return &command.StickerInfo{
		FileSHA256:     fileSHA,
		AudioSHA256:    audioSHA,
		IsAudioSticker: hasAudio,
		IsAnimated:     st.GetIsAnimated(),
	}
}

func unwrapStickerMessage(message *waE2E.Message, viaAudioWrapper bool, depth int) (*waE2E.StickerMessage, bool) {
	if message == nil || depth > 6 {
		return nil, false
	}
	if st := message.GetStickerMessage(); st != nil {
		return st, viaAudioWrapper
	}
	if inner := message.GetAudioStickerMessage().GetMessage(); inner != nil {
		if st, hasAudio := unwrapStickerMessage(inner, true, depth+1); st != nil {
			return st, hasAudio
		}
	}
	for _, inner := range []*waE2E.Message{
		message.GetEphemeralMessage().GetMessage(),
		message.GetViewOnceMessage().GetMessage(),
		message.GetViewOnceMessageV2().GetMessage(),
		message.GetViewOnceMessageV2Extension().GetMessage(),
		message.GetLottieStickerMessage().GetMessage(),
		message.GetDeviceSentMessage().GetMessage(),
	} {
		if inner != nil {
			if st, hasAudio := unwrapStickerMessage(inner, viaAudioWrapper, depth+1); st != nil {
				return st, hasAudio
			}
		}
	}
	return nil, false
}

// StripQuotedContext returns a copy of the message without quoted message context.
// Used to retry sending when sender key distribution fails for quoted replies in groups.
func StripQuotedContext(msg *waE2E.Message) *waE2E.Message {
	if msg == nil {
		return nil
	}
	// Clone the message to avoid modifying the original
	cloned := proto.Clone(msg).(*waE2E.Message)

	// Strip ContextInfo with QuotedMessage from all message types that support it
	for _, target := range []*waE2E.ContextInfo{
		cloned.GetExtendedTextMessage().GetContextInfo(),
		cloned.GetImageMessage().GetContextInfo(),
		cloned.GetVideoMessage().GetContextInfo(),
		cloned.GetPtvMessage().GetContextInfo(),
		cloned.GetAudioMessage().GetContextInfo(),
		cloned.GetDocumentMessage().GetContextInfo(),
		cloned.GetStickerMessage().GetContextInfo(),
	} {
		if target != nil {
			target.QuotedMessage = nil
		}
	}

	// Also handle AudioStickerMessage which has nested StickerMessage
	if cloned.GetAudioStickerMessage() != nil && cloned.GetAudioStickerMessage().GetMessage() != nil {
		if stickerMsg := cloned.GetAudioStickerMessage().GetMessage().GetStickerMessage(); stickerMsg != nil {
			if ctx := stickerMsg.GetContextInfo(); ctx != nil {
				ctx.QuotedMessage = nil
			}
		}
	}

	return cloned
}
