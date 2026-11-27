package welcome

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/SafwanGanz/Sonicos/internal/moderation"
	"github.com/SafwanGanz/Sonicos/internal/welcomecard"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

type SettingsStore interface {
	GetWelcomeSettings(ctx context.Context, groupJID string) (moderation.WelcomeSettings, error)
}

type Service struct {
	client      *whatsmeow.Client
	store       SettingsStore
	logger      *slog.Logger
	avatarCache map[string][]byte
}

func NewService(client *whatsmeow.Client, store SettingsStore, logger *slog.Logger) *Service {
	if client == nil || store == nil || logger == nil {
		return nil
	}
	return &Service{
		client:      client,
		store:       store,
		logger:      logger,
		avatarCache: make(map[string][]byte),
	}
}

func (s *Service) HandleGroupInfo(ctx context.Context, evt *events.GroupInfo) error {
	if s == nil || s.client == nil || s.store == nil {
		return nil
	}

	if len(evt.Join) == 0 {
		return nil
	}

	groupJID := evt.JID.String()
	settings, err := s.store.GetWelcomeSettings(ctx, groupJID)
	if err != nil {
		s.logger.Error("get welcome settings", "group", groupJID, "error", err)
		return nil
	}
	if !settings.Enabled {
		return nil
	}

	groupInfo, err := s.client.GetGroupInfo(ctx, evt.JID)
	if err != nil {
		s.logger.Error("get group info for welcome", "group", groupJID, "error", err)
		return nil
	}

	for _, joinJID := range evt.Join {
		avatarBytes, err := s.getAvatarBytes(ctx, joinJID)
		if err != nil {
			s.logger.Debug("get avatar for welcome", "user", joinJID.String(), "error", err)
		}

		welcomeText := "Welcome"
		if settings.CustomText != "" {
			welcomeText = settings.CustomText
		}

		opts := welcomecard.Options{
			Width:       340,
			Height:      180,
			Scale:       2,
			Username:    s.getDisplayName(joinJID, groupInfo),
			ServerName:  groupInfo.Name,
			WelcomeText: welcomeText,
			AvatarBytes: avatarBytes,
		}

		pngData, err := welcomecard.GeneratePNG(opts)
		if err != nil {
			s.logger.Error("generate welcome card", "user", joinJID.String(), "error", err)
			continue
		}

		if err := s.sendWelcomeImage(ctx, evt.JID, pngData); err != nil {
			s.logger.Error("send welcome card", "group", groupJID, "user", joinJID.String(), "error", err)
		}
	}

	return nil
}

func (s *Service) sendWelcomeImage(ctx context.Context, chatJID types.JID, pngData []byte) error {
	upload, err := s.client.Upload(ctx, pngData, whatsmeow.MediaImage)
	if err != nil {
		return fmt.Errorf("upload welcome image: %w", err)
	}

	msg := &waE2E.ImageMessage{
		URL:               proto.String(upload.URL),
		Mimetype:          proto.String("image/png"),
		FileSHA256:        upload.FileSHA256,
		FileLength:        proto.Uint64(upload.FileLength),
		MediaKey:          upload.MediaKey,
		FileEncSHA256:     upload.FileEncSHA256,
		DirectPath:        proto.String(upload.DirectPath),
		MediaKeyTimestamp: proto.Int64(time.Now().Unix()),
	}

	_, err = s.client.SendMessage(ctx, chatJID, &waE2E.Message{
		ImageMessage: msg,
	})
	return err
}

func (s *Service) getAvatarBytes(ctx context.Context, jid types.JID) ([]byte, error) {
	jidStr := jid.String()
	if cached, ok := s.avatarCache[jidStr]; ok {
		return cached, nil
	}

	params := &whatsmeow.GetProfilePictureParams{Preview: true}
	pic, err := s.client.GetProfilePictureInfo(ctx, jid, params)
	if err != nil {
		return nil, err
	}
	if pic == nil || pic.URL == "" {
		return nil, fmt.Errorf("no avatar URL")
	}

	resp, err := http.DefaultClient.Get(pic.URL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("avatar fetch status %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	s.avatarCache[jidStr] = data
	return data, nil
}

func (s *Service) getDisplayName(jid types.JID, groupInfo *types.GroupInfo) string {
	for _, p := range groupInfo.Participants {
		if p.JID == jid {
			if p.DisplayName != "" {
				return p.DisplayName
			}
			return jid.User
		}
	}
	if s.client.Store != nil && s.client.Store.ID != nil && jid == *s.client.Store.ID {
		return "You"
	}
	return jid.User
}