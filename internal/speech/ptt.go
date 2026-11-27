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

type PTTSynthesizer interface {
	Synthesizer
	SynthesizePTT(ctx context.Context, text string) (ogg []byte, seconds uint32, waveform []byte, err error)
}

func (e *EdgeTTS) SynthesizePTT(ctx context.Context, text string) ([]byte, uint32, []byte, error) {
	mp3, err := e.Synthesize(ctx, text)
	if err != nil {
		return nil, 0, nil, err
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, 0, nil, fmt.Errorf("ffmpeg not found: %w", err)
	}
	id := time.Now().UnixNano()
	in := filepath.Join(os.TempDir(), fmt.Sprintf("sonicos_ptt_in_%d.mp3", id))
	out := filepath.Join(os.TempDir(), fmt.Sprintf("sonicos_ptt_out_%d.ogg", id))
	if err := os.WriteFile(in, mp3, 0o600); err != nil {
		return nil, 0, nil, err
	}
	defer os.Remove(in)
	defer os.Remove(out)

	cmd := exec.CommandContext(ctx, ffmpeg, "-y", "-i", in, "-c:a", "libopus", "-ac", "1", "-ar", "48000", "-b:a", "64k", "-application", "voip", out)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, 0, nil, fmt.Errorf("ffmpeg opus conversion: %s", strings.TrimSpace(stderr.String()))
	}
	ogg, err := os.ReadFile(out)
	if err != nil {
		return nil, 0, nil, err
	}
	return ogg, probeDurationSeconds(ctx, ffmpeg, out), nil, nil
}

func probeDurationSeconds(ctx context.Context, ffmpeg, path string) uint32 {
	cmd := exec.CommandContext(ctx, ffmpeg, "-i", path)
	var out bytes.Buffer
	cmd.Stderr = &out
	_ = cmd.Run()
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, "Duration:") {
			fields := strings.Fields(line)
			for i, f := range fields {
				if f == "Duration:" && i+1 < len(fields) {
					var h, m, s int
					if _, err := fmt.Sscanf(fields[i+1], "%d:%d:%d", &h, &m, &s); err == nil {
						return uint32(h*3600 + m*60 + s)
					}
				}
			}
		}
	}
	return 1
}
