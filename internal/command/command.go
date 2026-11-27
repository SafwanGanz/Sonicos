package command

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/SafwanGanz/Sonicos/internal/aimode"
)

type StickerInfo struct {
	FileSHA256     string `json:"file_sha256,omitempty"`
	AudioSHA256    string `json:"audio_sha256,omitempty"`
	IsAudioSticker bool   `json:"is_audio_sticker,omitempty"`
	IsAnimated     bool   `json:"is_animated,omitempty"`
}

type BannedSticker struct {
	FileSHA256  string `json:"file_sha256,omitempty"`
	AudioSHA256 string `json:"audio_sha256,omitempty"`
	Reason      string `json:"reason,omitempty"`
	AddedBy     string `json:"added_by,omitempty"`
	AddedAt     int64  `json:"added_at,omitempty"`
}

type ModerationStore interface {
	BanSticker(ctx context.Context, groupJID string, entry BannedSticker) error
	UnbanSticker(ctx context.Context, groupJID, fileSHA256, audioSHA256 string) (bool, error)
	UnbanStickerByPrefix(ctx context.Context, groupJID, hashPrefix string) (bool, error)
	IsStickerBanned(ctx context.Context, groupJID, fileSHA256, audioSHA256 string) (BannedSticker, bool, error)
	ListBannedStickers(ctx context.Context, groupJID string) ([]BannedSticker, error)
	AddStrike(ctx context.Context, groupJID, userID string) (int, error)
	GetStrikes(ctx context.Context, groupJID, userID string) (int, error)
	ResetStrikes(ctx context.Context, groupJID, userID string) error
	RememberSticker(ctx context.Context, groupJID, messageID string, info StickerInfo) error
	LookupSticker(ctx context.Context, groupJID, messageID string) (StickerInfo, bool, error)
	GetShields(ctx context.Context, groupJID string) (GroupShields, error)
	SetShield(ctx context.Context, groupJID, field, value string) error
	IncrementRate(ctx context.Context, groupJID, bucket, userID string, windowSec int) (int, error)
	SaveStage(ctx context.Context, scopeKey string, media StagedMedia, ttlSec int) error
	LoadStage(ctx context.Context, scopeKey string) (StagedMedia, bool, error)
	ClearStage(ctx context.Context, scopeKey string) error
	SaveReactionSticker(ctx context.Context, ownerID, emoji string, sticker SavedReactionSticker) error
	GetReactionSticker(ctx context.Context, ownerID, emoji string) (SavedReactionSticker, bool, error)
	DeleteReactionSticker(ctx context.Context, ownerID, emoji string) (bool, error)
	ListReactionStickers(ctx context.Context, ownerID string) ([]SavedReactionSticker, error)
	SetWelcomeEnabled(ctx context.Context, groupJID string, enabled bool) error
}

type AIModeStore interface {
	GetState(ctx context.Context) (aimode.State, error)
	SetState(ctx context.Context, enabled bool, owner string) error
	GetGroupEnabled(ctx context.Context, groupJID string) (bool, error)
	SetGroupEnabled(ctx context.Context, groupJID string, enabled bool) error
}

type SavedReactionSticker struct {
	Emoji    string `json:"emoji"`
	Data     []byte `json:"data"`
	Animated bool   `json:"animated,omitempty"`
	SavedAt  int64  `json:"saved_at,omitempty"`
}

type GroupShields struct {
	AntiLoud         bool    `json:"anti_loud"`
	AntiLoudDB       float64 `json:"anti_loud_db"`
	AntiLink         bool    `json:"anti_link"`
	AntiSpam         bool    `json:"anti_spam"`
	AntiStickerFlood bool    `json:"anti_sticker_flood"`
}

type StagedMedia struct {
	Kind     MediaKind `json:"kind"`
	MIMEType string    `json:"mime_type"`
	Data     []byte    `json:"data"`
	Animated bool      `json:"animated,omitempty"`
}

type Message struct {
	ID                 string
	Chat               string
	Sender             string
	FromMe             bool
	Text               string
	Group              bool
	MentionedJIDs      []string
	QuotedSender       string
	QuotedMessageID    string
	QuotedFromMe       bool
	Sticker            *StickerInfo
	QuotedSticker      *StickerInfo
	MediaSource        MediaSource
	StickerAudioSource MediaSource
	GroupService       GroupService
	ModerationStore    ModerationStore
	AIModeStore        AIModeStore
}

type MediaKind string

const (
	MediaImage   MediaKind = "image"
	MediaVideo   MediaKind = "video"
	MediaAudio   MediaKind = "audio"
	MediaSticker MediaKind = "sticker"
)

type Attachment struct {
	Kind     MediaKind
	MIMEType string
	Data     []byte
}

type MediaSource interface {
	Download(context.Context) (Attachment, error)
}

type Sticker struct {
	Data          []byte
	MIMEType      string
	Animated      bool
	ExifOffset    uint32
	Emojis        []string
	Audio         bool
	AudioData     []byte
	AudioMIMEType string
	Seconds       uint32
	Waveform      []byte
}

type ImageReply struct {
	Data     []byte
	MIMEType string
	Caption  string
}

type AudioReply struct {
	Data     []byte
	MIMEType string
	Seconds  uint32
	Waveform []byte
	PTT      bool
}

type VideoNoteReply struct {
	Data    []byte
	Thumb   []byte
	Seconds uint32
	Width   uint32
	Height  uint32
}

type VideoReply struct {
	Data    []byte
	Thumb   []byte
	Caption string
	Seconds uint32
	Width   uint32
	Height  uint32
}

type StickerReaction struct {
	Sticker           Sticker
	TargetStanzaID    string
	TargetParticipant string
	TargetFromMe      bool
}

type Reply struct {
	Text            string
	Mentions        []string
	Sticker         *Sticker
	Image           *ImageReply
	Audio           *AudioReply
	Video           *VideoReply
	VideoNote       *VideoNoteReply
	StickerReaction *StickerReaction
}

type ParticipantAction string

const (
	ParticipantAdd     ParticipantAction = "add"
	ParticipantRemove  ParticipantAction = "remove"
	ParticipantPromote ParticipantAction = "promote"
	ParticipantDemote  ParticipantAction = "demote"
)

type GroupParticipant struct {
	JID          string
	PhoneNumber  string
	IsAdmin      bool
	IsSuperAdmin bool
}

type GroupInfo struct {
	JID          string
	Name         string
	Topic        string
	Owner        string
	BotJID       string
	Locked       bool
	Announce     bool
	Approval     bool
	EphemeralSec uint32
	MemberAddAll bool
	Participants []GroupParticipant
}

type GroupRequest struct {
	JID         string
	RequestedAt int64
}

type GroupService interface {
	GetInfo(ctx context.Context, groupJID string) (GroupInfo, error)
	UpdateParticipants(ctx context.Context, groupJID string, targets []string, action ParticipantAction) error
	SetName(ctx context.Context, groupJID, name string) error
	SetDescription(ctx context.Context, groupJID, description string) error
	SetPhoto(ctx context.Context, groupJID string, image []byte) error
	SetAnnounce(ctx context.Context, groupJID string, announce bool) error
	SetLocked(ctx context.Context, groupJID string, locked bool) error
	SetJoinApproval(ctx context.Context, groupJID string, requireApproval bool) error
	SetMemberAddMode(ctx context.Context, groupJID string, allowAll bool) error
	SetDisappearing(ctx context.Context, groupJID string, seconds uint32) error
	GetInviteLink(ctx context.Context, groupJID string, reset bool) (string, error)
	GetJoinRequests(ctx context.Context, groupJID string) ([]GroupRequest, error)
	UpdateJoinRequests(ctx context.Context, groupJID string, targets []string, approve bool) error
	RevokeMessage(ctx context.Context, chatJID, senderJID, messageID string) error
	Leave(ctx context.Context, groupJID string) error
}

type Handler func(context.Context, Message, []string, ReplyFunc) error

type Watcher func(context.Context, Message, ReplyFunc) error

type ReplyFunc func(context.Context, Reply) error

type Command struct {
	Name        string
	Aliases     []string
	Category    string
	Description string
	Handler     Handler
}

type Registry struct {
	mu       sync.RWMutex
	commands map[string]Command
	watchers []Watcher
}

func NewRegistry() *Registry {
	return &Registry{commands: make(map[string]Command)}
}

func (r *Registry) Watch(w Watcher) {
	if w == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.watchers = append(r.watchers, w)
}

func (r *Registry) Watchers() []Watcher {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Watcher, len(r.watchers))
	copy(out, r.watchers)
	return out
}

func (r *Registry) Register(cmd Command) error {
	cmd.Name = normalize(cmd.Name)
	if cmd.Name == "" || cmd.Handler == nil {
		return errors.New("command name and handler are required")
	}

	keys := append([]string{cmd.Name}, cmd.Aliases...)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, key := range keys {
		key = normalize(key)
		if key == "" {
			return errors.New("command aliases cannot be empty")
		}
		if _, exists := r.commands[key]; exists {
			return fmt.Errorf("command %q is already registered", key)
		}
	}
	for _, key := range keys {
		key = normalize(key)
		r.commands[key] = cmd
	}
	return nil
}

func (r *Registry) Dispatch(ctx context.Context, prefix string, msg Message, reply ReplyFunc) (bool, error) {
	text := strings.TrimSpace(msg.Text)
	if !strings.HasPrefix(text, prefix) {
		return false, nil
	}
	parts := strings.Fields(strings.TrimSpace(strings.TrimPrefix(text, prefix)))
	if len(parts) == 0 {
		return false, nil
	}

	r.mu.RLock()
	cmd, exists := r.commands[normalize(parts[0])]
	r.mu.RUnlock()
	if !exists {
		return false, nil
	}
	return true, cmd.Handler(ctx, msg, parts[1:], reply)
}

func (r *Registry) Lookup(name string) (Command, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cmd, ok := r.commands[normalize(name)]
	return cmd, ok
}

func (r *Registry) Commands() []Command {
	r.mu.RLock()
	defer r.mu.RUnlock()

	commands := make([]Command, 0, len(r.commands))
	seen := make(map[string]struct{}, len(r.commands))
	for _, cmd := range r.commands {
		if _, exists := seen[cmd.Name]; exists {
			continue
		}
		seen[cmd.Name] = struct{}{}
		commands = append(commands, cmd)
	}
	return commands
}

func normalize(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}
