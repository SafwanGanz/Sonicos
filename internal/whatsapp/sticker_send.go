package whatsapp

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"github.com/SafwanGanz/Sonicos/internal/command"
	stickerpkg "github.com/SafwanGanz/Sonicos/internal/sticker"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func (b *Bot) encodeSticker(ctx context.Context, sticker command.Sticker, source *events.Message) (*waE2E.Message, error) {

	if sticker.Audio && len(sticker.AudioData) > 0 {
		stickerUp, err := b.client.Upload(ctx, sticker.Data, whatsmeow.MediaImage)
		if err != nil {
			return nil, fmt.Errorf("upload sound sticker WebP: %w", err)
		}
		audioUp, err := b.client.Upload(ctx, sticker.AudioData, whatsmeow.MediaAudio)
		if err != nil {
			return nil, fmt.Errorf("upload sound sticker audio: %w", err)
		}
		exifOffset := sticker.ExifOffset
		if exifOffset == 0 {
			exifOffset = stickerpkg.FindWebpExifOffset(sticker.Data)
		}
		var firstFrameSidecar []byte
		if sticker.Animated && exifOffset > 0 {
			firstFrameSidecar, err = computeFirstFrameSidecar(sticker.Data, stickerUp.MediaKey, exifOffset, stickerUp.FileEncSHA256)
			if err != nil {
				return nil, err
			}
		}
		return buildSoundStickerMessage(sticker, stickerUp, audioUp, exifOffset, firstFrameSidecar, source)
	}

	if sticker.Audio {
		upload, err := b.client.Upload(ctx, sticker.Data, whatsmeow.MediaAudio)
		if err != nil {
			return nil, fmt.Errorf("upload audio media: %w", err)
		}
		return buildAudioOnlyMessage(sticker, upload, source), nil
	}

	upload, err := b.client.Upload(ctx, sticker.Data, whatsmeow.MediaImage)
	if err != nil {
		return nil, fmt.Errorf("upload sticker media: %w", err)
	}
	exifOffset := sticker.ExifOffset
	if exifOffset == 0 {
		exifOffset = stickerpkg.FindWebpExifOffset(sticker.Data)
	}
	var firstFrameSidecar []byte
	if sticker.Animated && exifOffset > 0 {
		firstFrameSidecar, err = computeFirstFrameSidecar(sticker.Data, upload.MediaKey, exifOffset, upload.FileEncSHA256)
		if err != nil {
			return nil, err
		}
	}
	return buildSilentStickerMessage(sticker, upload, exifOffset, firstFrameSidecar, source), nil
}

func buildSoundStickerMessage(
	sticker command.Sticker,
	stickerUp whatsmeow.UploadResponse,
	audioUp whatsmeow.UploadResponse,
	exifOffset uint32,
	firstFrameSidecar []byte,
	source *events.Message,
) (*waE2E.Message, error) {
	sec := sticker.Seconds
	if sec < 1 {
		sec = 1
	}
	aMime := strings.TrimSpace(sticker.AudioMIMEType)
	if aMime == "" {
		aMime = "audio/ogg; codecs=opus"
	}
	sMime := strings.TrimSpace(sticker.MIMEType)
	if sMime == "" {
		sMime = "image/webp"
	}

	nowSec := time.Now().Unix()
	nowMs := time.Now().UnixMilli()

	nestedAudio := &waE2E.AudioMessage{
		URL:               proto.String(audioUp.URL),
		Mimetype:          proto.String(aMime),
		FileSHA256:        audioUp.FileSHA256,
		FileLength:        proto.Uint64(audioUp.FileLength),
		Seconds:           proto.Uint32(sec),
		MediaKey:          audioUp.MediaKey,
		FileEncSHA256:     audioUp.FileEncSHA256,
		DirectPath:        proto.String(audioUp.DirectPath),
		MediaKeyTimestamp: proto.Int64(nowSec),
		ContextInfo: &waE2E.ContextInfo{
			MediaDomainInfo: &waE2E.MediaDomainInfo{
				MediaKeyDomain: waE2E.MediaKeyDomain_MEDIA_KEY_DOMAIN_E2EE.Enum(),
				E2EeMediaKey:   audioUp.MediaKey,
			},
		},
	}
	if wave := normalizeWaveform(sticker.Waveform); len(wave) > 0 {
		nestedAudio.Waveform = wave
	}

	var stickerCtx *waE2E.ContextInfo
	if source != nil {
		stickerCtx = quotedContextLite(source)
	} else {
		stickerCtx = &waE2E.ContextInfo{}
	}
	stickerCtx.MediaDomainInfo = &waE2E.MediaDomainInfo{
		MediaKeyDomain: waE2E.MediaKeyDomain_MEDIA_KEY_DOMAIN_E2EE.Enum(),
		E2EeMediaKey:   stickerUp.MediaKey,
	}

	cleanEmojis := make([]string, 0, len(sticker.Emojis))
	for _, em := range sticker.Emojis {
		if t := strings.TrimSpace(em); t != "" {
			cleanEmojis = append(cleanEmojis, t)
		}
	}
	if len(cleanEmojis) == 0 {
		cleanEmojis = []string{"🔊"}
	}

	st := &waE2E.StickerMessage{
		URL:               proto.String(stickerUp.URL),
		FileSHA256:        stickerUp.FileSHA256,
		FileEncSHA256:     stickerUp.FileEncSHA256,
		MediaKey:          stickerUp.MediaKey,
		Mimetype:          proto.String(sMime),
		DirectPath:        proto.String(stickerUp.DirectPath),
		FileLength:        proto.Uint64(stickerUp.FileLength),
		MediaKeyTimestamp: proto.Int64(nowSec),
		IsAnimated:        proto.Bool(sticker.Animated),
		ContextInfo:       stickerCtx,
		StickerSentTS:     proto.Int64(nowMs),
		IsAvatar:          proto.Bool(false),
		IsAiSticker:       proto.Bool(false),
		IsLottie:          proto.Bool(false),
		Premium:           proto.Int32(0),
		Emojis:            proto.String(strings.Join(cleanEmojis, ",")),
		Audio:             &waE2E.StickerMessage_AudioMessage{AudioMessage: nestedAudio},
	}
	if sticker.Animated && exifOffset > 0 {
		st.FirstFrameLength = proto.Uint32(exifOffset)
		if len(firstFrameSidecar) == 10 {
			st.FirstFrameSidecar = firstFrameSidecar
		}
	}

	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("generate messageSecret: %w", err)
	}

	return &waE2E.Message{
		MessageContextInfo: &waE2E.MessageContextInfo{
			MessageSecret: secret,
		},
		AudioStickerMessage: &waE2E.FutureProofMessage{
			Message: &waE2E.Message{
				StickerMessage: st,
			},
		},
	}, nil
}

func buildSilentStickerMessage(
	sticker command.Sticker,
	upload whatsmeow.UploadResponse,
	exifOffset uint32,
	firstFrameSidecar []byte,
	source *events.Message,
) *waE2E.Message {
	var contextInfo *waE2E.ContextInfo
	if source != nil {
		contextInfo = quotedContext(source)
	}
	mime := strings.TrimSpace(sticker.MIMEType)
	if mime == "" {
		mime = "image/webp"
	}
	st := &waE2E.StickerMessage{
		URL:                proto.String(upload.URL),
		FileSHA256:         upload.FileSHA256,
		FileEncSHA256:      upload.FileEncSHA256,
		MediaKey:           upload.MediaKey,
		Mimetype:           proto.String(mime),
		DirectPath:         proto.String(upload.DirectPath),
		FileLength:         proto.Uint64(upload.FileLength),
		MediaKeyTimestamp:  proto.Int64(time.Now().Unix()),
		ContextInfo:        contextInfo,
		AccessibilityLabel: proto.String("Sticker"),
		Width:              proto.Uint32(512),
		Height:             proto.Uint32(512),
		IsAnimated:         proto.Bool(sticker.Animated),
		StickerSentTS:      proto.Int64(time.Now().UnixMilli()),
	}
	if sticker.Animated && exifOffset > 0 && len(firstFrameSidecar) == 10 {
		st.FirstFrameLength = proto.Uint32(exifOffset)
		st.FirstFrameSidecar = firstFrameSidecar
	}
	if len(sticker.Emojis) > 0 {
		st.Emojis = proto.String(strings.Join(sticker.Emojis, ","))
	}
	return &waE2E.Message{StickerMessage: st}
}

func buildAudioOnlyMessage(sticker command.Sticker, upload whatsmeow.UploadResponse, source *events.Message) *waE2E.Message {
	var contextInfo *waE2E.ContextInfo
	if source != nil {
		contextInfo = quotedContext(source)
	}
	mime := strings.TrimSpace(sticker.MIMEType)
	if mime == "" {
		mime = "audio/ogg; codecs=opus"
	}
	audio := &waE2E.AudioMessage{
		URL:               proto.String(upload.URL),
		Mimetype:          proto.String(mime),
		FileSHA256:        upload.FileSHA256,
		FileLength:        proto.Uint64(upload.FileLength),
		Seconds:           proto.Uint32(sticker.Seconds),
		MediaKey:          upload.MediaKey,
		FileEncSHA256:     upload.FileEncSHA256,
		DirectPath:        proto.String(upload.DirectPath),
		MediaKeyTimestamp: proto.Int64(time.Now().Unix()),
		PTT:               proto.Bool(false),
		ContextInfo:       contextInfo,
	}
	if wave := normalizeWaveform(sticker.Waveform); len(wave) > 0 {
		audio.Waveform = wave
	}
	return &waE2E.Message{AudioMessage: audio}
}

func pkcs7Pad(src []byte, blockSize int) []byte {
	padLen := blockSize - (len(src) % blockSize)
	out := make([]byte, len(src)+padLen)
	copy(out, src)
	for i := len(src); i < len(out); i++ {
		out[i] = byte(padLen)
	}
	return out
}

func computeFirstFrameSidecar(plainBuf, mediaKey []byte, firstFrameLen uint32, expectedFileEncSha256 []byte) ([]byte, error) {
	if len(mediaKey) != 32 {
		return nil, fmt.Errorf("invalid mediaKey length %d", len(mediaKey))
	}
	expanded, err := hkdf.Key(sha256.New, mediaKey, nil, string(whatsmeow.MediaImage), 112)
	if err != nil {
		return nil, fmt.Errorf("hkdf key expansion failed: %w", err)
	}
	iv := expanded[0:16]
	cipherKey := expanded[16:48]
	macKey := expanded[48:80]

	block, err := aes.NewCipher(cipherKey)
	if err != nil {
		return nil, fmt.Errorf("aes cipher init failed: %w", err)
	}
	padded := pkcs7Pad(plainBuf, aes.BlockSize)
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, padded)

	if len(expectedFileEncSha256) > 0 {
		macH := hmac.New(sha256.New, macKey)
		macH.Write(iv)
		macH.Write(ct)
		fullMac := macH.Sum(nil)[:10]

		shaH := sha256.New()
		shaH.Write(ct)
		shaH.Write(fullMac)
		encSha := shaH.Sum(nil)
		if !bytes.Equal(encSha, expectedFileEncSha256) {
			return nil, fmt.Errorf("sticker fileEncSha256 mismatch while computing firstFrameSidecar")
		}
	}

	blockEnd := len(ct)
	if firstFrameLen > 0 {
		target := ((int(firstFrameLen) + 15) / 16) * 16
		if target < blockEnd {
			blockEnd = target
		}
	}
	sidecarH := hmac.New(sha256.New, macKey)
	sidecarH.Write(iv)
	sidecarH.Write(ct[:blockEnd])
	return sidecarH.Sum(nil)[:10], nil
}

func normalizeWaveform(raw []byte) []byte {
	if len(raw) == 0 {
		return nil
	}
	out := make([]byte, 64)
	n := min(len(raw), 64)
	for i := range n {
		v := raw[i]
		if v > 100 {
			v = 100
		}
		out[i] = v
	}
	return out
}

func quotedContext(evt *events.Message) *waE2E.ContextInfo {
	return &waE2E.ContextInfo{
		StanzaID:      proto.String(string(evt.Info.ID)),
		Participant:   proto.String(evt.Info.Sender.String()),
		RemoteJID:     proto.String(evt.Info.Chat.String()),
		QuotedMessage: evt.Message,
	}
}

func quotedContextLite(evt *events.Message) *waE2E.ContextInfo {
	preview := extractText(evt.Message)
	if preview == "" {
		preview = "Media"
	}
	return &waE2E.ContextInfo{
		StanzaID:    proto.String(string(evt.Info.ID)),
		Participant: proto.String(evt.Info.Sender.ToNonAD().String()),
		QuotedMessage: &waE2E.Message{
			Conversation: proto.String(preview),
		},
	}
}

func (b *Bot) encodeImageReply(ctx context.Context, img command.ImageReply, source *events.Message) (*waE2E.Message, error) {
	upload, err := b.client.Upload(ctx, img.Data, whatsmeow.MediaImage)
	if err != nil {
		return nil, fmt.Errorf("upload image reply: %w", err)
	}
	mime := strings.TrimSpace(img.MIMEType)
	if mime == "" {
		mime = "image/png"
	}
	var contextInfo *waE2E.ContextInfo
	if source != nil {
		contextInfo = quotedContext(source)
	}
	msg := &waE2E.ImageMessage{
		URL:               proto.String(upload.URL),
		Mimetype:          proto.String(mime),
		FileSHA256:        upload.FileSHA256,
		FileLength:        proto.Uint64(upload.FileLength),
		MediaKey:          upload.MediaKey,
		FileEncSHA256:     upload.FileEncSHA256,
		DirectPath:        proto.String(upload.DirectPath),
		MediaKeyTimestamp: proto.Int64(time.Now().Unix()),
		ContextInfo:       contextInfo,
	}
	if strings.TrimSpace(img.Caption) != "" {
		msg.Caption = proto.String(img.Caption)
	}
	return &waE2E.Message{ImageMessage: msg}, nil
}

func (b *Bot) encodeAudioReply(ctx context.Context, audio command.AudioReply, source *events.Message) (*waE2E.Message, error) {
	upload, err := b.client.Upload(ctx, audio.Data, whatsmeow.MediaAudio)
	if err != nil {
		return nil, fmt.Errorf("upload audio reply: %w", err)
	}
	var contextInfo *waE2E.ContextInfo
	if source != nil {
		contextInfo = quotedContext(source)
	}
	mime := strings.TrimSpace(audio.MIMEType)
	if mime == "" {
		mime = "audio/ogg; codecs=opus"
	}
	sec := audio.Seconds
	if sec < 1 {
		sec = 1
	}
	msg := &waE2E.AudioMessage{
		URL:               proto.String(upload.URL),
		Mimetype:          proto.String(mime),
		FileSHA256:        upload.FileSHA256,
		FileLength:        proto.Uint64(upload.FileLength),
		Seconds:           proto.Uint32(sec),
		MediaKey:          upload.MediaKey,
		FileEncSHA256:     upload.FileEncSHA256,
		DirectPath:        proto.String(upload.DirectPath),
		MediaKeyTimestamp: proto.Int64(time.Now().Unix()),
		PTT:               proto.Bool(audio.PTT),
		ContextInfo:       contextInfo,
	}
	if wave := normalizeWaveform(audio.Waveform); len(wave) > 0 {
		msg.Waveform = wave
	}
	return &waE2E.Message{AudioMessage: msg}, nil
}

func (b *Bot) encodeVideoReply(ctx context.Context, video command.VideoReply, source *events.Message) (*waE2E.Message, error) {
	upload, err := b.client.Upload(ctx, video.Data, whatsmeow.MediaVideo)
	if err != nil {
		return nil, fmt.Errorf("upload video reply: %w", err)
	}
	return buildVideoMessage(video, upload, source), nil
}

func buildVideoMessage(video command.VideoReply, upload whatsmeow.UploadResponse, source *events.Message) *waE2E.Message {
	var contextInfo *waE2E.ContextInfo
	if source != nil {
		contextInfo = quotedContext(source)
	}
	sec := video.Seconds
	if sec < 1 {
		sec = 1
	}
	w := video.Width
	if w == 0 {
		w = 512
	}
	h := video.Height
	if h == 0 {
		h = 512
	}
	videoMsg := &waE2E.VideoMessage{
		URL:               proto.String(upload.URL),
		DirectPath:        proto.String(upload.DirectPath),
		MediaKey:          upload.MediaKey,
		FileEncSHA256:     upload.FileEncSHA256,
		FileSHA256:        upload.FileSHA256,
		FileLength:        proto.Uint64(upload.FileLength),
		Mimetype:          proto.String("video/mp4"),
		Seconds:           proto.Uint32(sec),
		Width:             proto.Uint32(w),
		Height:            proto.Uint32(h),
		MediaKeyTimestamp: proto.Int64(time.Now().Unix()),
		ContextInfo:       contextInfo,
	}
	if len(video.Thumb) > 0 {
		videoMsg.JPEGThumbnail = video.Thumb
	}
	if strings.TrimSpace(video.Caption) != "" {
		videoMsg.Caption = proto.String(video.Caption)
	}
	return &waE2E.Message{VideoMessage: videoMsg}
}

func (b *Bot) encodeVideoNoteReply(ctx context.Context, note command.VideoNoteReply, source *events.Message) (*waE2E.Message, error) {
	upload, err := b.client.Upload(ctx, note.Data, whatsmeow.MediaVideo)
	if err != nil {
		return nil, fmt.Errorf("upload video note: %w", err)
	}
	return buildVideoNoteMessage(note, upload, source), nil
}

func buildVideoNoteMessage(note command.VideoNoteReply, upload whatsmeow.UploadResponse, source *events.Message) *waE2E.Message {
	var contextInfo *waE2E.ContextInfo
	if source != nil {
		contextInfo = quotedContext(source)
	}
	sec := note.Seconds
	if sec < 1 {
		sec = 1
	} else if sec > 60 {
		sec = 60
	}
	w := note.Width
	if w == 0 {
		w = 384
	}
	h := note.Height
	if h == 0 {
		h = 384
	}
	videoMsg := &waE2E.VideoMessage{
		URL:               proto.String(upload.URL),
		DirectPath:        proto.String(upload.DirectPath),
		MediaKey:          upload.MediaKey,
		FileEncSHA256:     upload.FileEncSHA256,
		FileSHA256:        upload.FileSHA256,
		FileLength:        proto.Uint64(upload.FileLength),
		Mimetype:          proto.String("video/mp4"),
		Seconds:           proto.Uint32(sec),
		Width:             proto.Uint32(w),
		Height:            proto.Uint32(h),
		MediaKeyTimestamp: proto.Int64(time.Now().Unix()),
		ContextInfo:       contextInfo,
	}
	if len(note.Thumb) > 0 {
		videoMsg.JPEGThumbnail = note.Thumb
	}
	return &waE2E.Message{PtvMessage: videoMsg}
}

func (b *Bot) encodeStickerReaction(ctx context.Context, react command.StickerReaction, chatJID types.JID) (*waE2E.Message, error) {
	upload, err := b.client.Upload(ctx, react.Sticker.Data, whatsmeow.MediaImage)
	if err != nil {
		return nil, fmt.Errorf("upload sticker reaction: %w", err)
	}
	exifOffset := react.Sticker.ExifOffset
	if exifOffset == 0 {
		exifOffset = stickerpkg.FindWebpExifOffset(react.Sticker.Data)
	}
	var firstFrameSidecar []byte
	if react.Sticker.Animated && exifOffset > 0 {
		firstFrameSidecar, err = computeFirstFrameSidecar(react.Sticker.Data, upload.MediaKey, exifOffset, upload.FileEncSHA256)
		if err != nil {
			return nil, err
		}
	}
	return buildStickerReactionMessage(react, upload, exifOffset, firstFrameSidecar, chatJID)
}

func buildStickerReactionMessage(
	react command.StickerReaction,
	upload whatsmeow.UploadResponse,
	exifOffset uint32,
	firstFrameSidecar []byte,
	chatJID types.JID,
) (*waE2E.Message, error) {
	st := buildSilentStickerMessage(react.Sticker, upload, exifOffset, firstFrameSidecar, nil).GetStickerMessage()
	parentKey := &waCommon.MessageKey{
		RemoteJID: proto.String(chatJID.ToNonAD().String()),
		FromMe:    proto.Bool(react.TargetFromMe),
		ID:        proto.String(strings.TrimSpace(react.TargetStanzaID)),
	}
	if chatJID.Server == types.GroupServer && !react.TargetFromMe {
		if p := strings.TrimSpace(react.TargetParticipant); p != "" {
			if parsed, err := types.ParseJID(p); err == nil && !parsed.IsEmpty() {
				parentKey.Participant = proto.String(parsed.ToNonAD().String())
			} else {
				parentKey.Participant = proto.String(p)
			}
		}
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("generate messageSecret: %w", err)
	}
	return &waE2E.Message{
		StickerMessage: st,
		MessageContextInfo: &waE2E.MessageContextInfo{
			MessageSecret: secret,
			MessageAssociation: &waE2E.MessageAssociation{
				AssociationType:  waE2E.MessageAssociation_STICKER_ANNOTATION.Enum(),
				ParentMessageKey: parentKey,
			},
		},
	}, nil
}
