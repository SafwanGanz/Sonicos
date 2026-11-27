package transcript

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Result struct {
	Text      string
	Language  string
	Uncertain bool
	Provider  string
}

type Provider interface {
	Name() string
	Available() bool
	Transcribe(ctx context.Context, audio []byte, mime string) (Result, error)
}

type Engine struct {
	providers []Provider
	logger    *slog.Logger
	maxSize   int64
	maxDur    time.Duration
}

func NewEngine(logger *slog.Logger, providers ...Provider) *Engine {
	if logger == nil {
		logger = slog.Default()
	}
	available := make([]Provider, 0, len(providers))
	for _, p := range providers {
		if p != nil && p.Available() {
			available = append(available, p)
		}
	}
	return &Engine{providers: available, logger: logger, maxSize: 16 << 20, maxDur: 10 * time.Minute}
}

func (e *Engine) Available() bool { return e != nil && len(e.providers) > 0 }

func (e *Engine) Providers() []string {
	if e == nil {
		return nil
	}
	out := make([]string, 0, len(e.providers))
	for _, p := range e.providers {
		out = append(out, p.Name())
	}
	return out
}

var ErrNoProvider = errors.New("no transcription provider is configured")

func (e *Engine) Transcribe(ctx context.Context, audio []byte, mime string) (Result, error) {
	if e == nil || len(e.providers) == 0 {
		return Result{}, ErrNoProvider
	}
	if len(audio) == 0 {
		return Result{}, errors.New("empty audio payload")
	}
	if int64(len(audio)) > e.maxSize {
		return Result{}, fmt.Errorf("audio exceeds %d MB limit", e.maxSize>>20)
	}
	prepared, preparedMime, err := e.Prepare(ctx, audio, mime)
	if err != nil {
		return Result{}, err
	}
	audio, mime = prepared, preparedMime

	var lastErr error
	for _, p := range e.providers {
		res, err := p.Transcribe(ctx, audio, mime)
		if err == nil && strings.TrimSpace(res.Text) != "" {
			res.Provider = p.Name()
			return res, nil
		}
		if err != nil {
			lastErr = err
			e.logger.Warn("transcription provider failed; trying next", "provider", p.Name(), "error", err)
		} else {
			lastErr = errors.New("provider returned empty transcription")
			e.logger.Warn("transcription provider returned empty text", "provider", p.Name())
		}
	}
	if lastErr == nil {
		lastErr = errors.New("transcription failed")
	}
	return Result{}, fmt.Errorf("all transcription providers failed: %w", lastErr)
}

func (e *Engine) Prepare(ctx context.Context, audio []byte, mime string) ([]byte, string, error) {
	mime = strings.ToLower(strings.TrimSpace(mime))
	if strings.Contains(mime, "ogg") || strings.Contains(mime, "opus") {
		return audio, "audio/ogg", nil
	}
	if seconds, err := probeDuration(ctx, audio, mime); err == nil && seconds > e.maxDur {
		return nil, "", fmt.Errorf("audio duration %.0fs exceeds limit", seconds.Seconds())
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {

		return audio, mime, nil
	}
	in := filepath.Join(os.TempDir(), fmt.Sprintf("sonicos_stt_in_%d", time.Now().UnixNano()))
	out := in + ".ogg"
	if err := os.WriteFile(in, audio, 0o600); err != nil {
		return nil, "", fmt.Errorf("stage audio: %w", err)
	}
	defer os.Remove(in)
	defer os.Remove(out)
	cmd := exec.CommandContext(ctx, ffmpeg, "-y", "-i", in, "-ac", "1", "-ar", "16000", "-c:a", "libopus", "-b:a", "32k", out)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, "", fmt.Errorf("audio conversion failed: %s", strings.TrimSpace(stderr.String()))
	}
	converted, err := os.ReadFile(out)
	if err != nil {
		return nil, "", fmt.Errorf("read converted audio: %w", err)
	}
	if seconds, err := probeDuration(ctx, converted, "audio/ogg"); err == nil && seconds > e.maxDur {
		return nil, "", fmt.Errorf("audio duration %.0fs exceeds limit", seconds.Seconds())
	}
	return converted, "audio/ogg", nil
}

func probeDuration(ctx context.Context, audio []byte, mime string) (time.Duration, error) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return 0, err
	}
	in := filepath.Join(os.TempDir(), fmt.Sprintf("sonicos_stt_probe_%d", time.Now().UnixNano()))
	if err := os.WriteFile(in, audio, 0o600); err != nil {
		return 0, err
	}
	defer os.Remove(in)
	cmd := exec.CommandContext(ctx, ffmpeg, "-i", in)
	var out strings.Builder
	cmd.Stderr = &out
	_ = cmd.Run()
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, "Duration:") {
			fields := strings.Fields(line)
			for i, f := range fields {
				if f == "Duration:" && i+1 < len(fields) {
					var h, m, s int
					if _, err := fmt.Sscanf(fields[i+1], "%d:%d:%d", &h, &m, &s); err == nil {
						return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second, nil
					}
				}
			}
		}
	}
	return 0, errors.New("duration unknown")
}
