package plugins

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/SafwanGanz/Sonicos/internal/command"
	"github.com/SafwanGanz/Sonicos/internal/downloader"
)

func init() {
	command.Register(command.Command{
		Name:        "ig",
		Aliases:     []string{"insta", "instagram"},
		Category:    "Media",
		Description: "Download Instagram reel/post media. Usage: !ig <url>",
		Handler:     instagramHandler,
	})
}

func instagramHandler(ctx context.Context, message command.Message, args []string, reply command.ReplyFunc) error {
	if len(args) == 0 {
		return reply(ctx, command.Reply{Text: "Usage: !ig <instagram_url>"})
	}

	rawURL := strings.TrimSpace(args[0])
	if rawURL == "" {
		return reply(ctx, command.Reply{Text: "Usage: !ig <instagram_url>"})
	}
	if !strings.Contains(rawURL, "instagram.com") {
		return reply(ctx, command.Reply{Text: "Please provide a valid Instagram URL"})
	}

	dl := downloader.NewDownloader(30 * time.Second)
	defer dl.Close()

	results, err := dl.Extract(ctx, rawURL)
	if err != nil {
		slog.Warn("instagram direct API failed, trying yt-dlp", "url", rawURL, "error", err)

		return downloadInstagramWithYTDLP(ctx, rawURL, reply)
	}

	const maxFileSize = 25 << 20

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

		caption := fmt.Sprintf("%s\n📥 Instagram via Sonicos", result.Title)

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

	return reply(ctx, command.Reply{Text: "No media found"})
}

func downloadInstagramWithYTDLP(ctx context.Context, url string, reply command.ReplyFunc) error {
	tmpDir, err := os.MkdirTemp("", "ytdlp-ig-*")
	if err != nil {
		return reply(ctx, command.Reply{Text: fmt.Sprintf("Download failed: %v", err)})
	}
	defer os.RemoveAll(tmpDir)

	outputTemplate := filepath.Join(tmpDir, "%(title)s.%(ext)s")

	ytArgs := []string{
		"-f", "best[ext=mp4]/best",
		"--recode-video", "mp4",
		"--postprocessor-args", "ffmpeg:-movflags +faststart -c:v libx264 -preset ultrafast -c:a aac -b:a 128k",
		"--max-filesize", "25M",
		"-o", outputTemplate,
		"--no-playlist",
		"--no-warnings",
		"--ignore-errors",
		"--no-simulate",
		url,
	}

	dlCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	cmd := exec.CommandContext(dlCtx, "yt-dlp", ytArgs...)
	cmd.Dir = tmpDir

	output, err := cmd.CombinedOutput()
	if err != nil {
		errMsg := fmt.Sprintf("yt-dlp failed: %v\n%s", err, truncateOutput(string(output)))
		slog.Error("instagram yt-dlp failed", "url", url, "error", errMsg)
		return reply(ctx, command.Reply{Text: fmt.Sprintf("Download failed: %s", errMsg)})
	}

	files, err := os.ReadDir(tmpDir)
	if err != nil {
		return reply(ctx, command.Reply{Text: fmt.Sprintf("Failed to read downloaded file: %v", err)})
	}

	var downloadedFile string
	for _, f := range files {
		if !f.IsDir() {
			downloadedFile = filepath.Join(tmpDir, f.Name())
			break
		}
	}

	if downloadedFile == "" {
		return reply(ctx, command.Reply{Text: "No file downloaded"})
	}

	info, err := os.Stat(downloadedFile)
	if err != nil {
		return reply(ctx, command.Reply{Text: fmt.Sprintf("Failed to stat downloaded file: %v", err)})
	}

	if info.Size() > maxFileSize {
		return reply(ctx, command.Reply{Text: fmt.Sprintf("File too large (%s)", formatSize(info.Size()))})
	}

	data, err := os.ReadFile(downloadedFile)
	if err != nil {
		return reply(ctx, command.Reply{Text: fmt.Sprintf("Failed to read downloaded file: %v", err)})
	}

	ext := strings.ToLower(filepath.Ext(downloadedFile))
	if ext == ".mp4" || ext == ".mov" || ext == ".webm" {
		return reply(ctx, command.Reply{
			Video: &command.VideoReply{
				Data:    data,
				Caption: "Instagram via Sonicos",
				Seconds: 0,
				Width:   0,
				Height:  0,
			},
		})
	}

	return reply(ctx, command.Reply{
		Image: &command.ImageReply{
			Data:     data,
			MIMEType: "image/jpeg",
			Caption:  "Instagram via Sonicos",
		},
	})
}
