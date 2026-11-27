package speech

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Synthesizer interface {
	Name() string
	Available() bool
	Synthesize(ctx context.Context, text string) ([]byte, error)
}

type EdgeTTS struct {
	Voice string
}

func NewEdgeTTS(voice string) *EdgeTTS {
	if _, err := lookEdgeTTS(); err != nil {
		return nil
	}
	if strings.TrimSpace(voice) == "" {
		voice = "en-US-AriaNeural"
	}
	return &EdgeTTS{Voice: voice}
}

func lookEdgeTTS() (string, error) {
	candidates := []string{
		filepath.Join(os.Getenv("HOME"), ".local", "bin", "edge-tts"),
		"/usr/local/bin/edge-tts",
		"/usr/bin/edge-tts",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return exec.LookPath("edge-tts")
}

func (e *EdgeTTS) Name() string { return "edge-tts" }

func (e *EdgeTTS) Available() bool { return e != nil }

func (e *EdgeTTS) Synthesize(ctx context.Context, text string) ([]byte, error) {
	if e == nil {
		return nil, fmt.Errorf("edge-tts provider is not configured")
	}
	bin, err := lookEdgeTTS()
	if err != nil {
		return nil, fmt.Errorf("edge-tts CLI not found: %w", err)
	}
	id := time.Now().UnixNano()
	txtFile := filepath.Join(os.TempDir(), fmt.Sprintf("sonicos_tts_%d.txt", id))
	if err := os.WriteFile(txtFile, []byte(text), 0o600); err != nil {
		return nil, err
	}
	defer os.Remove(txtFile)
	tmpFile := filepath.Join(os.TempDir(), fmt.Sprintf("sonicos_tts_%d.mp3", id))
	defer os.Remove(tmpFile)

	cmd := exec.CommandContext(ctx, bin, "--voice", e.Voice, "--file", txtFile, "--write-media", tmpFile, "--no-progress")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("edge-tts: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return os.ReadFile(tmpFile)
}
