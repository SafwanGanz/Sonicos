package plugins

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/SafwanGanz/Sonicos/internal/command"
	"github.com/SafwanGanz/Sonicos/internal/downloader"
)

func init() {
	command.Register(command.Command{
		Name:        "dl",
		Aliases:     []string{"download"},
		Category:    "Media",
		Description: "Download media from supported sites. Usage: !dl <url> [quality]",
		Handler:     downloadHandler,
	})
}

var dlInstance *downloader.Downloader

func getDownloader() *downloader.Downloader {
	if dlInstance == nil {
		dlInstance = downloader.NewDownloader(30 * time.Second)
	}
	return dlInstance
}

func downloadHandler(ctx context.Context, message command.Message, args []string, reply command.ReplyFunc) error {
	if len(args) == 0 {
		services := getDownloader().GetSupportedServices()
		return reply(ctx, command.Reply{Text: fmt.Sprintf("Usage: !dl <url> [quality]\nSupported: %s", strings.Join(services, ", "))})
	}

	rawURL := strings.TrimSpace(args[0])
	if rawURL == "" {
		services := getDownloader().GetSupportedServices()
		return reply(ctx, command.Reply{Text: fmt.Sprintf("Usage: !dl <url> [quality]\nSupported: %s", strings.Join(services, ", "))})
	}

	dl := getDownloader()
	results, err := dl.Extract(ctx, rawURL)
	if err != nil {
		slog.Error("download failed", "url", rawURL, "error", err)
		return reply(ctx, command.Reply{Text: fmt.Sprintf("Download failed: %v", err)})
	}

	if len(results) == 0 {
		return reply(ctx, command.Reply{Text: "No media found"})
	}

	for _, result := range results {
		if result.URL == "" {
			continue
		}
		if result.Filesize > maxFileSize && result.MediaType == downloader.MediaTypeVideo {
			continue
		}

		data, mimeType, err := downloadMedia(ctx, dl, result.URL)
		if err != nil {
			slog.Error("download media failed", "url", result.URL, "error", err)
			continue
		}

		caption := fmt.Sprintf("%s\n📥 via Sonicos", result.Title)

		switch result.MediaType {
		case downloader.MediaTypeVideo:
			return reply(ctx, command.Reply{
				Video: &command.VideoReply{
					Data:    data,
					Caption: caption,
					Seconds: uint32(result.Duration),
					Width:   getWidth(result.Metadata),
					Height:  getHeight(result.Metadata),
				},
			})
		case downloader.MediaTypeAudio:
			return reply(ctx, command.Reply{
				Audio: &command.AudioReply{
					Data:     data,
					MIMEType: mimeType,
				},
			})
		case downloader.MediaTypeImage:
			return reply(ctx, command.Reply{
				Image: &command.ImageReply{
					Data:     data,
					MIMEType: mimeType,
					Caption:  caption,
				},
			})
		}
	}

	return reply(ctx, command.Reply{Text: "No suitable media found (files may be too large)"})
}