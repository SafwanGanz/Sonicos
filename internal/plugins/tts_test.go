package plugins

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SafwanGanz/Sonicos/internal/command"
)

func TestParseVoiceTableAndDefaultLookup(t *testing.T) {
	sample := strings.Join([]string{
		"Name                             Gender    ContentCategories      VoicePersonalities",
		"-------------------------------  --------  ---------------------  --------------------------------------",
		"en-US-AriaNeural                 Female    News, Novel            Positive, Confident",
		"en-US-GuyNeural                  Male      News, Novel            Passion",
		"ml-IN-MidhunNeural               Male      General                Friendly, Positive",
	}, "\n")

	voices := parseVoiceTable(sample)
	if len(voices) != 3 {
		t.Fatalf("expected 3 voices, got %d", len(voices))
	}

	byAlias, byShort := buildLookups(voices)
	reg := &ttsRegistry{
		voices:   voices,
		byAlias:  byAlias,
		byShort:  byShort,
		loadedAt: time.Now(),
		loaded:   true,
	}

	defVoice, ok := reg.find(context.Background(), "")
	if !ok {
		t.Fatal("expected default en-US-AriaNeural voice to be found")
	}
	if defVoice.ShortName != "en-US-AriaNeural" {
		t.Fatalf("expected en-US-AriaNeural, got %q", defVoice.ShortName)
	}

	if v, ok := reg.find(context.Background(), "midhun"); !ok || v.ShortName != "ml-IN-MidhunNeural" {
		t.Fatalf("expected midhun alias to resolve to ml-IN-MidhunNeural, got ok=%v v=%+v", ok, v)
	}
}

func TestVoices24hDoesNotCacheErrors(t *testing.T) {
	origBin, hadBin := os.LookupEnv("SONICOS_TTS_BIN")
	t.Cleanup(func() {
		if hadBin {
			_ = os.Setenv("SONICOS_TTS_BIN", origBin)
		} else {
			_ = os.Unsetenv("SONICOS_TTS_BIN")
		}
	})

	_ = os.Setenv("SONICOS_TTS_BIN", filepath.Join(t.TempDir(), "missing-edge-tts"))

	reg := &ttsRegistry{}
	if _, err := reg.voices24h(context.Background()); err == nil {
		t.Fatal("expected error from missing binary")
	}
	if reg.loaded {
		t.Fatal("expected failed voices24h call not to mark registry as loaded")
	}
}

func TestFFmpegBinaryEnvPrecedence(t *testing.T) {
	t.Setenv("FFMPEG_PATH", "/custom/ffmpeg-path")
	t.Setenv("FFMPEG", "/custom/ffmpeg")
	if got := ffmpegBinary(); got != "/custom/ffmpeg-path" {
		t.Fatalf("expected FFMPEG_PATH to win, got %q", got)
	}

	t.Setenv("FFMPEG_PATH", "")
	if got := ffmpegBinary(); got != "/custom/ffmpeg" {
		t.Fatalf("expected FFMPEG fallback, got %q", got)
	}

	t.Setenv("FFMPEG", "")
	if got := ffmpegBinary(); got != "ffmpeg" {
		t.Fatalf("expected default ffmpeg, got %q", got)
	}
}

func TestTTSSynthBinaryDynamicHomeLookup(t *testing.T) {
	t.Setenv("SONICOS_TTS_BIN", "")
	fakeHome := t.TempDir()
	binDir := filepath.Join(fakeHome, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeBin := filepath.Join(binDir, "edge-tts")
	if err := os.WriteFile(fakeBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", fakeHome)

	if got := ttsSynthBinary(); got != fakeBin {
		t.Fatalf("expected %q, got %q", fakeBin, got)
	}
}

func TestTTSRandIDConcurrencySafe(t *testing.T) {
	var wg sync.WaitGroup
	ids := make(chan int64, 64)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 8 {
				ids <- ttsRandID()
			}
		}()
	}
	wg.Wait()
	close(ids)

	seen := make(map[int64]bool, 64)
	for id := range ids {
		if seen[id] {
			t.Fatalf("duplicate ttsRandID generated: %d", id)
		}
		seen[id] = true
	}
}

func TestTTSListVoicesPaginationAndSynthesizeFlagSafety(t *testing.T) {
	origVoices := ttsReg.voices
	origAlias := ttsReg.byAlias
	origShort := ttsReg.byShort
	origLoadedAt := ttsReg.loadedAt
	origLoaded := ttsReg.loaded
	origErr := ttsReg.loadError
	t.Cleanup(func() {
		ttsReg.mu.Lock()
		ttsReg.voices = origVoices
		ttsReg.byAlias = origAlias
		ttsReg.byShort = origShort
		ttsReg.loadedAt = origLoadedAt
		ttsReg.loaded = origLoaded
		ttsReg.loadError = origErr
		ttsReg.mu.Unlock()
	})

	voices := make([]ttsVoice, 30)
	for i := range voices {
		name := "en-US-Voice" + string(rune('A'+i/26)) + string(rune('a'+i%26)) + "Neural"
		voices[i] = ttsVoice{
			ShortName: name,
			Alias:     "voice" + string(rune('A'+i/26)) + string(rune('a'+i%26)),
			Gender:    "Female",
			Locale:    "en-US",
		}
	}
	byAlias, byShort := buildLookups(voices)
	ttsReg.mu.Lock()
	ttsReg.voices = voices
	ttsReg.byAlias = byAlias
	ttsReg.byShort = byShort
	ttsReg.loadedAt = time.Now()
	ttsReg.loaded = true
	ttsReg.loadError = nil
	ttsReg.mu.Unlock()

	var replies []string
	replyFn := func(_ context.Context, r command.Reply) error {
		replies = append(replies, r.Text)
		return nil
	}

	if err := ttsListVoices(context.Background(), "en 2", replyFn, "!"); err != nil {
		t.Fatalf("ttsListVoices failed: %v", err)
	}
	if len(replies) != 1 || !strings.Contains(replies[0], "2/2") {
		t.Fatalf("expected page 2/2 output, got: %v", replies)
	}

	scriptDir := t.TempDir()
	script := filepath.Join(scriptDir, "fake-edge-tts")
	body := `#!/bin/sh
set -eu
out=""
infile=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --text)
      echo "unexpected --text flag" >&2
      exit 2
      ;;
    --file)
      infile="$2"
      shift 2
      ;;
    --write-media)
      out="$2"
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done
[ -n "$infile" ] && [ -n "$out" ]
cat "$infile" > "$out"
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SONICOS_TTS_BIN", script)

	got, err := synthesizeTTS(context.Background(), "- leading dash text", "en-US-AriaNeural")
	if err != nil {
		t.Fatalf("synthesizeTTS failed: %v", err)
	}
	if string(got) != "- leading dash text" {
		t.Fatalf("expected synthesized payload to match input text, got %q", string(got))
	}
}
