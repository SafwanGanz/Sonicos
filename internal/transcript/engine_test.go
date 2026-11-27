package transcript

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type stubProvider struct {
	name     string
	avail    bool
	res      Result
	err      error
	gotAudio []byte
	gotMime  string
}

func (s *stubProvider) Name() string    { return s.name }
func (s *stubProvider) Available() bool { return s.avail }
func (s *stubProvider) Transcribe(ctx context.Context, audio []byte, mime string) (Result, error) {
	s.gotAudio = audio
	s.gotMime = mime
	return s.res, s.err
}

func TestEngineFallsBackToNextProvider(t *testing.T) {
	p1 := &stubProvider{name: "p1", avail: true, err: errors.New("down")}
	p2 := &stubProvider{name: "p2", avail: true, res: Result{Text: "hello"}}
	engine := NewEngine(testLogger(), p1, p2)
	res, err := engine.Transcribe(context.Background(), []byte("audio"), "audio/ogg")
	if err != nil || res.Text != "hello" || res.Provider != "p2" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
}

func TestEngineSkipsUnavailable(t *testing.T) {
	p1 := &stubProvider{name: "p1", avail: false, res: Result{Text: "wrong"}}
	p2 := &stubProvider{name: "p2", avail: true, res: Result{Text: "right"}}
	engine := NewEngine(testLogger(), p1, p2)
	if got := engine.Providers(); len(got) != 1 || got[0] != "p2" {
		t.Fatalf("providers = %v", got)
	}
	res, err := engine.Transcribe(context.Background(), []byte("audio"), "audio/ogg")
	if err != nil || res.Text != "right" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
}

func TestEngineAllFail(t *testing.T) {
	p1 := &stubProvider{name: "p1", avail: true, err: errors.New("boom")}
	engine := NewEngine(testLogger(), p1)
	_, err := engine.Transcribe(context.Background(), []byte("audio"), "audio/ogg")
	if err == nil || !strings.Contains(err.Error(), "all transcription providers failed") {
		t.Fatalf("err = %v", err)
	}
}

func TestEngineNoProvider(t *testing.T) {
	engine := NewEngine(testLogger())
	if engine.Available() {
		t.Fatal("engine without providers must be unavailable")
	}
	_, err := engine.Transcribe(context.Background(), []byte("audio"), "audio/ogg")
	if !errors.Is(err, ErrNoProvider) {
		t.Fatalf("err = %v", err)
	}
}

func TestEngineEmptyAudio(t *testing.T) {
	p := &stubProvider{name: "p", avail: true}
	engine := NewEngine(testLogger(), p)
	_, err := engine.Transcribe(context.Background(), nil, "audio/ogg")
	if err == nil {
		t.Fatal("expected empty audio error")
	}
}

func TestPrepareOggPassthrough(t *testing.T) {
	engine := NewEngine(testLogger())
	data, mime, err := engine.Prepare(context.Background(), []byte("OggSdata"), "audio/ogg; codecs=opus")
	if err != nil || string(data) != "OggSdata" || mime != "audio/ogg" {
		t.Fatalf("data = %q, mime = %q, err = %v", data, mime, err)
	}
}
