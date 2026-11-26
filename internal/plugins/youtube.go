package plugins

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/SafwanGanz/Sonicos/internal/command"
)

const maxFileSize = 25 << 20 // 25MB WhatsApp limit

func init() {
	command.Register(command.Command{
		Name:        "mp4",
		Aliases:     []string{"ytmp4", "video"},
		Category:    "Media",
		Description: "Download YouTube video as MP4. Usage: !mp4 <url>",
		Handler:     downloadMP4,
	})
	command.Register(command.Command{
		Name:        "mp3",
		Aliases:     []string{"ytmp3", "audio"},
		Category:    "Media",
		Description: "Download YouTube audio as MP3. Usage: !mp3 <url>",
		Handler:     downloadMP3,
	})
}

func downloadMP4(ctx context.Context, message command.Message, args []string, reply command.ReplyFunc) error {
	return downloadYTDLP(ctx, message, args, reply, "mp4")
}

func downloadMP3(ctx context.Context, message command.Message, args []string, reply command.ReplyFunc) error {
	return downloadYTDLP(ctx, message, args, reply, "mp3")
}

func downloadYTDLP(ctx context.Context, message command.Message, args []string, reply command.ReplyFunc, format string) error {
	if len(args) == 0 {
		cmd := "mp4"
		if format == "mp3" {
			cmd = "mp3"
		}
		return reply(ctx, command.Reply{Text: fmt.Sprintf("Usage: !%s <youtube_url>", cmd)})
	}

	rawURL := strings.TrimSpace(args[0])
	if !isValidYouTubeURL(rawURL) {
		return reply(ctx, command.Reply{Text: "Please provide a valid YouTube URL"})
	}

	// Create temp directory for download
	tmpDir, err := os.MkdirTemp("", "ytdlp-*")
	if err != nil {
		slog.Error("create temp dir", "error", err)
		return reply(ctx, command.Reply{Text: "Failed to create temp directory"})
	}
	defer os.RemoveAll(tmpDir)

	// Build yt-dlp command
	var outputTemplate string
	var ytArgs []string

	if format == "mp3" {
		outputTemplate = filepath.Join(tmpDir, "%(title)s.%(ext)s")
		ytArgs = []string{
			"-f", "bestaudio[ext=m4a]/bestaudio",
			"--extract-audio",
			"--audio-format", "mp3",
			"--audio-quality", "0",
			"-o", outputTemplate,
			"--no-playlist",
			"--no-warnings",
			rawURL,
		}
	} else {
		// MP4: best video+audio merged, under 25MB (WhatsApp limit)
		outputTemplate = filepath.Join(tmpDir, "%(title)s.%(ext)s")
		ytArgs = []string{
			"-f", "bestvideo[ext=mp4]+bestaudio[ext=m4a]/best[ext=mp4]/best[filesize<25M]/worst[ext=mp4]",
			"--max-filesize", "25M",
			"--merge-output-format", "mp4",
			"-o", outputTemplate,
			"--no-playlist",
			"--no-warnings",
			rawURL,
		}
	}

	// Add timeout context
	dlCtx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()

	cmd := exec.CommandContext(dlCtx, "yt-dlp", ytArgs...)
	cmd.Dir = tmpDir

	output, err := cmd.CombinedOutput()
	if err != nil {
		slog.Error("yt-dlp failed", "format", format, "error", err, "output", string(output))
		return reply(ctx, command.Reply{Text: fmt.Sprintf("Download failed: %v", err)})
	}

	// Find downloaded file
	files, err := os.ReadDir(tmpDir)
	if err != nil {
		return reply(ctx, command.Reply{Text: "Failed to read downloaded file"})
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

	// Check file size
	info, err := os.Stat(downloadedFile)
	if err != nil {
		return reply(ctx, command.Reply{Text: "Failed to stat downloaded file"})
	}

	if info.Size() > maxFileSize {
		return reply(ctx, command.Reply{Text: fmt.Sprintf("File too large (%s). WhatsApp limit is 25 MB.", formatSize(info.Size()))})
	}

	// Read file
	data, err := os.ReadFile(downloadedFile)
	if err != nil {
		return reply(ctx, command.Reply{Text: "Failed to read downloaded file"})
	}

	// Extract title from filename
	title := strings.TrimSuffix(filepath.Base(downloadedFile), filepath.Ext(downloadedFile))
	if format == "mp3" {
		title = strings.TrimSuffix(title, ".mp3")
	} else {
		title = strings.TrimSuffix(title, ".mp4")
	}

	caption := fmt.Sprintf("%s\n📥 %s via Sonicos", title, strings.ToUpper(format))

	if format == "mp3" {
		return reply(ctx, command.Reply{
			Audio: &command.AudioReply{
				Data:     data,
				MIMEType: "audio/mpeg",
			},
		})
	}

	return reply(ctx, command.Reply{
		Video: &command.VideoReply{
			Data:    data,
			Caption: caption,
		},
	})
}

func isValidYouTubeURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Host)
	return strings.Contains(host, "youtube.com") ||
		strings.Contains(host, "youtu.be") ||
		strings.Contains(host, "youtube-nocookie.com")
}

func formatSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// Check if yt-dlp is available
func checkYTDLP() error {
	_, err := exec.LookPath("yt-dlp")
	return err
}

// Initialize check
func init() {
	if err := checkYTDLP(); err != nil {
		slog.Warn("yt-dlp not found in PATH. Install it for !mp3/!mp4 commands to work", "error", err)
	}
}