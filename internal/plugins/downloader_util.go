package plugins

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/SafwanGanz/Sonicos/internal/downloader"
)

const maxFileSize = 25 << 20 // 25MB WhatsApp limit

func downloadMedia(ctx context.Context, dl *downloader.Downloader, mediaURL string) ([]byte, string, error) {
	// Check if it's a YouTube signed URL that needs special handling
	if strings.Contains(mediaURL, "googlevideo.com") || strings.Contains(mediaURL, "youtube.com") {
		return downloadWithYTDLP(ctx, mediaURL)
	}

	resp, err := dl.Client().Do(dl.NewRequest(ctx, "GET", mediaURL))
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("download failed with status: %d", resp.StatusCode)
	}

	data := make([]byte, 0)
	buf := make([]byte, 32768)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			data = append(data, buf[:n]...)
		}
		if err != nil {
			break
		}
		if len(data) > maxFileSize {
			return nil, "", fmt.Errorf("file too large")
		}
	}

	mimeType := resp.Header.Get("Content-Type")
	if mimeType == "" {
		if strings.HasSuffix(mediaURL, ".mp4") || strings.Contains(mediaURL, "video") {
			mimeType = "video/mp4"
		} else if strings.HasSuffix(mediaURL, ".mp3") || strings.Contains(mediaURL, "audio") {
			mimeType = "audio/mpeg"
		} else {
			mimeType = "image/jpeg"
		}
	}

	return data, mimeType, nil
}

func downloadWithYTDLP(ctx context.Context, mediaURL string) ([]byte, string, error) {
	tmpDir, err := os.MkdirTemp("", "ytdlp-dl-*")
	if err != nil {
		return nil, "", err
	}
	defer os.RemoveAll(tmpDir)

	outputTemplate := filepath.Join(tmpDir, "%(title)s.%(ext)s")

	// Try to detect if it's audio or video from URL
	isAudio := strings.Contains(mediaURL, "audio") || strings.Contains(mediaURL, "m4a") || strings.Contains(mediaURL, "opus")

	var ytArgs []string
	if isAudio {
		ytArgs = []string{
			"-f", "bestaudio[ext=m4a]/bestaudio[ext=webm]/bestaudio",
			"--extract-audio",
			"--audio-format", "mp3",
			"--audio-quality", "0",
			"-o", outputTemplate,
			"--no-playlist",
			"--no-warnings",
			"--ignore-errors",
			mediaURL,
		}
	} else {
		// Video: Force re-encode to H.264/AAC with proper MP4 container for WhatsApp
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
			mediaURL,
		}
	}

	dlCtx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()

	cmd := exec.CommandContext(dlCtx, "yt-dlp", ytArgs...)
	cmd.Dir = tmpDir

	output, err := cmd.CombinedOutput()
	if err != nil {
		// Fallback for video
		if !isAudio && strings.Contains(string(output), "Requested format is not available") {
			return downloadWithYTDLPFallback(dlCtx, tmpDir, mediaURL)
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
	if isAudio {
		mimeType = "audio/mpeg"
	}

	return data, mimeType, nil
}

func downloadWithYTDLPFallback(ctx context.Context, tmpDir, mediaURL string) ([]byte, string, error) {
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
		mediaURL,
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

func truncateOutput(s string) string {
	lines := strings.Split(s, "\n")
	if len(lines) > 5 {
		return strings.Join(lines[:5], "\n") + "\n... (truncated)"
	}
	return s
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

func getWidth(meta map[string]string) uint32 {
	if meta == nil {
		return 0
	}
	if w, ok := meta["width"]; ok {
		var val uint32
		fmt.Sscanf(w, "%d", &val)
		return val
	}
	return 0
}

func getHeight(meta map[string]string) uint32 {
	if meta == nil {
		return 0
	}
	if h, ok := meta["height"]; ok {
		var val uint32
		fmt.Sscanf(h, "%d", &val)
		return val
	}
	return 0
}