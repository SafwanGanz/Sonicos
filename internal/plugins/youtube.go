package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
			"-f", "bestaudio[ext=m4a]/bestaudio[ext=webm]/bestaudio",
			"--extract-audio",
			"--audio-format", "mp3",
			"--audio-quality", "0",
			"-o", outputTemplate,
			"--no-playlist",
			"--no-warnings",
			"--ignore-errors",
			rawURL,
		}
	} else {
		// MP4: Force re-encode to H.264/AAC with proper MP4 container for WhatsApp
		outputTemplate = filepath.Join(tmpDir, "%(title)s.%(ext)s")
		ytArgs = []string{
			"-f", "bestvideo+bestaudio/best",
			"-S", "vcodec:h264,acodec:aac,res:720,filesize",
			"--recode-video", "mp4",
			"--postprocessor-args", "ffmpeg:-movflags +faststart -c:v libx264 -profile:v high -level 4.0 -preset fast -c:a aac -b:a 128k",
			"--max-filesize", "25M",
			"-o", outputTemplate,
			"--no-playlist",
			"--no-warnings",
			"--ignore-errors",
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
		errMsg := string(output)
		slog.Error("yt-dlp failed", "format", format, "error", err, "output", errMsg)
		
		// Try fallback for MP4 if first attempt fails
		if format == "mp4" && strings.Contains(errMsg, "Requested format is not available") {
			return tryMP4Fallback(dlCtx, tmpDir, rawURL, reply)
		}
		
		return reply(ctx, command.Reply{Text: fmt.Sprintf("Download failed: %v\n%s", err, truncateOutput(errMsg))})
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

	// Extract video metadata (duration, width, height) using ffprobe
	var durationSec uint32
	var width, height uint32
	if format == "mp4" {
		durationSec, width, height = getVideoMetadata(downloadedFile)
		if durationSec == 0 {
			durationSec = 1 // fallback
		}
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
			Seconds: durationSec,
			Width:   width,
			Height:  height,
		},
	})
}

func tryMP4Fallback(ctx context.Context, tmpDir, rawURL string, reply command.ReplyFunc) error {
	// Fallback: try with simpler format selection but still force re-encode
	outputTemplate := filepath.Join(tmpDir, "%(title)s.%(ext)s")
	ytArgs := []string{
		"-f", "best[ext=mp4]/best",
		"--recode-video", "mp4",
		"--postprocessor-args", "ffmpeg:-movflags +faststart -c:v libx264 -profile:v high -level 4.0 -preset fast -c:a aac -b:a 128k",
		"--max-filesize", "25M",
		"-o", outputTemplate,
		"--no-playlist",
		"--no-warnings",
		"--ignore-errors",
		rawURL,
	}

	cmd := exec.CommandContext(ctx, "yt-dlp", ytArgs...)
	cmd.Dir = tmpDir

	output, err := cmd.CombinedOutput()
	if err != nil {
		slog.Error("yt-dlp fallback failed", "error", err, "output", string(output))
		return reply(ctx, command.Reply{Text: fmt.Sprintf("Download failed (fallback): %v", err)})
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

	info, err := os.Stat(downloadedFile)
	if err != nil {
		return reply(ctx, command.Reply{Text: "Failed to stat downloaded file"})
	}

	if info.Size() > maxFileSize {
		return reply(ctx, command.Reply{Text: fmt.Sprintf("File too large (%s). WhatsApp limit is 25 MB.", formatSize(info.Size()))})
	}

	data, err := os.ReadFile(downloadedFile)
	if err != nil {
		return reply(ctx, command.Reply{Text: "Failed to read downloaded file"})
	}

	// Extract video metadata
	durationSec, width, height := getVideoMetadata(downloadedFile)
	if durationSec == 0 {
		durationSec = 1
	}

	title := strings.TrimSuffix(filepath.Base(downloadedFile), filepath.Ext(downloadedFile))
	title = strings.TrimSuffix(title, ".mp4")

	caption := fmt.Sprintf("%s\n📥 MP4 via Sonicos", title)

	return reply(ctx, command.Reply{
		Video: &command.VideoReply{
			Data:    data,
			Caption: caption,
			Seconds: durationSec,
			Width:   width,
			Height:  height,
		},
	})
}

func truncateOutput(s string) string {
	lines := strings.Split(s, "\n")
	if len(lines) > 5 {
		return strings.Join(lines[:5], "\n") + "\n... (truncated)"
	}
	return s
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

// getVideoMetadata extracts duration, width, and height from a video file using ffprobe
func getVideoMetadata(filePath string) (durationSec uint32, width, height uint32) {
	cmd := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=width,height,duration", "-of", "json", filePath)
	output, err := cmd.Output()
	if err != nil {
		slog.Warn("ffprobe failed", "error", err, "file", filePath)
		return 0, 0, 0
	}

	var result struct {
		Streams []struct {
			Width    int     `json:"width"`
			Height   int     `json:"height"`
			Duration string  `json:"duration"`
		} `json:"streams"`
	}

	if err := json.Unmarshal(output, &result); err != nil {
		slog.Warn("failed to parse ffprobe output", "error", err)
		return 0, 0, 0
	}

	if len(result.Streams) > 0 {
		stream := result.Streams[0]
		if stream.Width > 0 && stream.Height > 0 {
			width = uint32(stream.Width)
			height = uint32(stream.Height)
		}
		if stream.Duration != "" {
			if d, err := strconv.ParseFloat(stream.Duration, 64); err == nil {
				durationSec = uint32(d)
			}
		}
	}

	return durationSec, width, height
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