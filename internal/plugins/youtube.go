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
)

func init() {
	command.Register(command.Command{
		Name:        "mp4",
		Aliases:     []string{"ytmp4", "video"},
		Category:    "Media",
		Description: "Download YouTube video as MP4. Usage: !mp4 <url>",
		Handler:     downloadMP4Handler,
	})
	command.Register(command.Command{
		Name:        "mp3",
		Aliases:     []string{"ytmp3", "audio"},
		Category:    "Media",
		Description: "Download YouTube audio as MP3. Usage: !mp3 <url>",
		Handler:     downloadMP3Handler,
	})
}

func downloadMP4Handler(ctx context.Context, message command.Message, args []string, reply command.ReplyFunc) error {
	if len(args) == 0 {
		return reply(ctx, command.Reply{Text: "Usage: !mp4 <youtube_url>"})
	}

	rawURL := strings.TrimSpace(args[0])
	if rawURL == "" {
		return reply(ctx, command.Reply{Text: "Usage: !mp4 <youtube_url>"})
	}

	data, _, err := downloadYTDLP(ctx, rawURL, false)
	if err != nil {
		slog.Error("youtube mp4 download failed", "url", rawURL, "error", err)
		return reply(ctx, command.Reply{Text: fmt.Sprintf("Download failed: %v", err)})
	}

	title := extractTitleFromURL(rawURL)
	caption := fmt.Sprintf("%s\n📥 MP4 via Sonicos", title)

	return reply(ctx, command.Reply{
		Video: &command.VideoReply{
			Data:     data,
			Caption:  caption,
			Seconds:  0,
			Width:    0,
			Height:   0,
		},
	})
}

func downloadMP3Handler(ctx context.Context, message command.Message, args []string, reply command.ReplyFunc) error {
	if len(args) == 0 {
		return reply(ctx, command.Reply{Text: "Usage: !mp3 <youtube_url>"})
	}

	rawURL := strings.TrimSpace(args[0])
	if rawURL == "" {
		return reply(ctx, command.Reply{Text: "Usage: !mp3 <youtube_url>"})
	}

	data, mimeType, err := downloadYTDLP(ctx, rawURL, true)
	if err != nil {
		slog.Error("youtube mp3 download failed", "url", rawURL, "error", err)
		return reply(ctx, command.Reply{Text: fmt.Sprintf("Download failed: %v", err)})
	}

	return reply(ctx, command.Reply{
		Audio: &command.AudioReply{
			Data:     data,
			MIMEType: mimeType,
		},
	})
}

func downloadYTDLP(ctx context.Context, url string, audioOnly bool) ([]byte, string, error) {
	tmpDir, err := os.MkdirTemp("", "ytdlp-*")
	if err != nil {
		return nil, "", err
	}
	defer os.RemoveAll(tmpDir)

	outputTemplate := filepath.Join(tmpDir, "%(title)s.%(ext)s")

	var ytArgs []string
	if audioOnly {
		ytArgs = []string{
			"-f", "bestaudio[ext=m4a]/bestaudio/best",
			"--extract-audio",
			"--audio-format", "mp3",
			"--audio-quality", "5",
			"-o", outputTemplate,
			"--no-playlist",
			"--no-warnings",
			"--ignore-errors",
			"--no-simulate",
			url,
		}
	} else {
		// Simpler format selection - try to get MP4 directly without heavy transcoding
		ytArgs = []string{
			"-f", "best[ext=mp4][height<=720][vcodec^=avc1]/best[ext=mp4]/best[height<=720]/best",
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
	}

	// Shorter timeout - 90 seconds max
	dlCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	cmd := exec.CommandContext(dlCtx, "yt-dlp", ytArgs...)
	cmd.Dir = tmpDir

	output, err := cmd.CombinedOutput()
	if err != nil {
		if !audioOnly && strings.Contains(string(output), "Requested format is not available") {
			return downloadYTDLPFallback(dlCtx, tmpDir, url)
		}
		return nil, "", fmt.Errorf("yt-dlp failed: %v\n%s", err, truncateOutput(string(output)))
	}

	files, err := os.ReadDir(tmpDir)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read downloaded file: %w", err)
	}

	var downloadedFile string
	for _, f := range files {
		if !f.IsDir() {
			downloadedFile = filepath.Join(tmpDir, f.Name())
			break
		}
	}

	if downloadedFile == "" {
		return nil, "", fmt.Errorf("no file downloaded")
	}

	info, err := os.Stat(downloadedFile)
	if err != nil {
		return nil, "", fmt.Errorf("failed to stat downloaded file: %w", err)
	}

	if info.Size() > maxFileSize {
		return nil, "", fmt.Errorf("file too large (%s)", formatSize(info.Size()))
	}

	data, err := os.ReadFile(downloadedFile)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read downloaded file: %w", err)
	}

	mimeType := "video/mp4"
	if audioOnly {
		mimeType = "audio/mpeg"
	}

	return data, mimeType, nil
}

func downloadYTDLPFallback(ctx context.Context, tmpDir, url string) ([]byte, string, error) {
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

	cmd := exec.CommandContext(ctx, "yt-dlp", ytArgs...)
	cmd.Dir = tmpDir

	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, "", fmt.Errorf("yt-dlp fallback failed: %v\n%s", err, truncateOutput(string(output)))
	}

	files, err := os.ReadDir(tmpDir)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read downloaded file: %w", err)
	}

	var downloadedFile string
	for _, f := range files {
		if !f.IsDir() {
			downloadedFile = filepath.Join(tmpDir, f.Name())
			break
		}
	}

	if downloadedFile == "" {
		return nil, "", fmt.Errorf("no file downloaded")
	}

	info, err := os.Stat(downloadedFile)
	if err != nil {
		return nil, "", fmt.Errorf("failed to stat downloaded file: %w", err)
	}

	if info.Size() > maxFileSize {
		return nil, "", fmt.Errorf("file too large (%s)", formatSize(info.Size()))
	}

	data, err := os.ReadFile(downloadedFile)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read downloaded file: %w", err)
	}

	return data, "video/mp4", nil
}

func extractTitleFromURL(url string) string {
	if strings.Contains(url, "youtu.be/") {
		parts := strings.Split(url, "youtu.be/")
		if len(parts) > 1 {
			return strings.Split(parts[1], "?")[0]
		}
	}
	if strings.Contains(url, "v=") {
		parts := strings.Split(url, "v=")
		if len(parts) > 1 {
			return strings.Split(parts[1], "&")[0]
		}
	}
	return "YouTube Video"
}