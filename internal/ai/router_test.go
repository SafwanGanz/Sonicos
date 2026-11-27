package ai

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/SafwanGanz/Sonicos/internal/aimode"
	"github.com/SafwanGanz/Sonicos/internal/command"
	"github.com/SafwanGanz/Sonicos/internal/speech"
	"github.com/SafwanGanz/Sonicos/internal/transcript"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type fakeModeStore struct {
	global  bool
	groupOn map[string]bool
	failGet bool
}

func (f *fakeModeStore) GetState(ctx context.Context) (aimode.State, error) {
	if f.failGet {
		return aimode.State{}, context.DeadlineExceeded
	}
	return aimode.State{Enabled: f.global, Owner: "42"}, nil
}
func (f *fakeModeStore) SetState(ctx context.Context, enabled bool, owner string) error {
	f.global = enabled
	return nil
}
func (f *fakeModeStore) GetGroupEnabled(ctx context.Context, groupJID string) (bool, error) {
	if f.failGet {
		return false, context.DeadlineExceeded
	}
	if f.groupOn == nil {
		return true, nil
	}
	return f.groupOn[groupJID], nil
}
func (f *fakeModeStore) SetGroupEnabled(ctx context.Context, groupJID string, enabled bool) error {
	if f.groupOn == nil {
		f.groupOn = map[string]bool{}
	}
	f.groupOn[groupJID] = enabled
	return nil
}

type fakeText struct {
	mu       sync.Mutex
	result   ChatResult
	err      error
	requests []ChatRequest
}

func (f *fakeText) Name() string    { return "fake" }
func (f *fakeText) Available() bool { return true }
func (f *fakeText) Chat(ctx context.Context, req ChatRequest) (ChatResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	return f.result, f.err
}
func (f *fakeText) ChatStream(ctx context.Context, req ChatRequest, onDelta func(string)) (ChatResult, error) {
	return f.Chat(ctx, req)
}

func (f *fakeText) lastRequest() ChatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return ChatRequest{}
	}
	return f.requests[len(f.requests)-1]
}

type fakeSTT struct {
	res transcript.Result
	err error
}

func (f *fakeSTT) Name() string    { return "fake-stt" }
func (f *fakeSTT) Available() bool { return true }
func (f *fakeSTT) Transcribe(ctx context.Context, audio []byte, mime string) (transcript.Result, error) {
	return f.res, f.err
}

type fakeTTS struct{ called bool }

func (f *fakeTTS) Name() string    { return "fake-tts" }
func (f *fakeTTS) Available() bool { return true }
func (f *fakeTTS) Synthesize(ctx context.Context, text string) ([]byte, error) {
	f.called = true
	return []byte("ID3fake-audio"), nil
}

func newTestRouter(t *testing.T, store command.AIModeStore, text TextProvider, stt transcript.Provider, tts *fakeTTS) (*Router, *command.Registry) {
	registry := command.NewRegistry()
	sttEngine := transcript.NewEngine(testLogger())
	if stt != nil {
		sttEngine = transcript.NewEngine(testLogger(), stt)
	}
	var synth speech.Synthesizer
	if tts != nil {
		synth = tts
	}
	router := NewRouter(store, text, sttEngine, synth, registry, testLogger(), 0)
	if router == nil {
		t.Fatal("NewRouter returned nil")
	}
	return router, registry
}

func TestAddressed(t *testing.T) {
	cases := []struct {
		text      string
		wantText  string
		wantMatch bool
	}{
		{"Sonicos, help me with this", "help me with this", true},
		{"sonicos turn this into a sticker", "turn this into a sticker", true},
		{"SONICOS: status?", "status?", true},
		{"  Sonicos   summarize our discussion ", "summarize our discussion", true},
		{"hey Sonicos what's up", "hey Sonicos what's up", false},
		{"!sticker", "!sticker", false},
		{"", "", false},
	}
	for _, tc := range cases {
		gotText, gotMatch := Addressed(tc.text)
		if gotMatch != tc.wantMatch || gotText != tc.wantText {
			t.Fatalf("Addressed(%q) = (%q, %v), want (%q, %v)", tc.text, gotText, gotMatch, tc.wantText, tc.wantMatch)
		}
	}
}

func TestRouterDefaultOff(t *testing.T) {
	router, _ := newTestRouter(t, &fakeModeStore{global: false}, &fakeText{}, nil, nil)
	handled, err := router.Handle(context.Background(), command.Message{
		Text: "Sonicos, hello", Chat: "x@s.whatsapp.net", Sender: "1@s.whatsapp.net",
	}, func(context.Context, command.Reply) error { return nil })
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if handled {
		t.Fatal("router must not engage while the persisted mode is OFF")
	}
}

func TestRouterFailSafeOnStoreError(t *testing.T) {
	router, _ := newTestRouter(t, &fakeModeStore{global: true, failGet: true}, &fakeText{}, nil, nil)
	handled, err := router.Handle(context.Background(), command.Message{
		Text: "Sonicos, hello", Chat: "x@s.whatsapp.net", Sender: "1@s.whatsapp.net",
	}, func(context.Context, command.Reply) error { return nil })
	if err != nil || handled {
		t.Fatalf("store errors must fail safe to disabled: handled = %v, err = %v", handled, err)
	}
}

func TestRouterIgnoresUnaddressed(t *testing.T) {
	router, _ := newTestRouter(t, &fakeModeStore{global: true}, &fakeText{}, nil, nil)
	handled, err := router.Handle(context.Background(), command.Message{
		Text: "just chatting", Chat: "x@g.us", Sender: "1@s.whatsapp.net", Group: true,
	}, func(context.Context, command.Reply) error { return nil })
	if err != nil || handled {
		t.Fatalf("ordinary conversation must not trigger AI: handled = %v, err = %v", handled, err)
	}
}

func TestRouterGroupOffOverrides(t *testing.T) {
	store := &fakeModeStore{global: true, groupOn: map[string]bool{"g@g.us": false}}
	router, _ := newTestRouter(t, store, &fakeText{}, nil, nil)
	handled, err := router.Handle(context.Background(), command.Message{
		Text: "Sonicos, hello", Chat: "g@g.us", Sender: "1@s.whatsapp.net", Group: true,
	}, func(context.Context, command.Reply) error { return nil })
	if err != nil || handled {
		t.Fatalf("group-level OFF must block AI: handled = %v, err = %v", handled, err)
	}
}

func TestRouterPlainReplyAndHistory(t *testing.T) {
	text := &fakeText{result: ChatResult{Text: "Sure, here is the summary."}}
	router, _ := newTestRouter(t, &fakeModeStore{global: true}, text, nil, nil)
	var sent command.Reply
	handled, err := router.Handle(context.Background(), command.Message{
		Text: "Sonicos, summarize our discussion", Chat: "x@g.us", Sender: "1@s.whatsapp.net", Group: true,
	}, func(_ context.Context, rep command.Reply) error { sent = rep; return nil })
	if err != nil || !handled || sent.Text != "Sure, here is the summary." {
		t.Fatalf("handled = %v, reply = %q, err = %v", handled, sent.Text, err)
	}

	_, _ = router.Handle(context.Background(), command.Message{
		Text: "Sonicos, and now translate it", Chat: "x@g.us", Sender: "1@s.whatsapp.net", Group: true,
	}, func(context.Context, command.Reply) error { return nil })
	req := text.lastRequest()
	if len(req.Messages) < 4 {
		t.Fatalf("history not supplied: %d messages", len(req.Messages))
	}
	joined := ""
	for _, m := range req.Messages {
		joined += m.Role + ":" + m.Content + "\n"
	}
	if !strings.Contains(joined, "user:summarize our discussion") || !strings.Contains(joined, "assistant:Sure, here is the summary.") {
		t.Fatalf("history missing prior turns:\n%s", joined)
	}
}

func TestRouterToolCall(t *testing.T) {
	text := &fakeText{result: ChatResult{ToolCalls: []ToolCall{{Name: "echo", Arguments: "hello world"}}}}
	router, registry := newTestRouter(t, &fakeModeStore{global: true}, text, nil, nil)
	var ranArgs []string
	if err := registry.Register(command.Command{
		Name: "echo", Description: "Echo text",
		Handler: func(_ context.Context, _ command.Message, args []string, reply command.ReplyFunc) error {
			ranArgs = args
			return reply(context.Background(), command.Reply{Text: "echo: " + strings.Join(args, " ")})
		},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	var sent command.Reply
	handled, err := router.Handle(context.Background(), command.Message{
		Text: "Sonicos, echo hello world", Chat: "x@s.whatsapp.net", Sender: "1@s.whatsapp.net",
	}, func(_ context.Context, rep command.Reply) error { sent = rep; return nil })
	if err != nil || !handled {
		t.Fatalf("handled = %v, err = %v", handled, err)
	}
	if strings.Join(ranArgs, " ") != "hello world" || sent.Text != "echo: hello world" {
		t.Fatalf("args = %v, reply = %q", ranArgs, sent.Text)
	}
}

func TestRouterVoiceTranscriptionAndVoiceReply(t *testing.T) {
	tts := &fakeTTS{}
	text := &fakeText{result: ChatResult{Text: "You said hello."}}
	router, _ := newTestRouter(t, &fakeModeStore{global: true}, text, &fakeSTT{res: transcript.Result{Text: "hello", Language: "English"}}, tts)
	var sent command.Reply
	handled, err := router.Handle(context.Background(), command.Message{
		Text: "Sonicos, what did I say?", Chat: "x@s.whatsapp.net", Sender: "1@s.whatsapp.net",
		MediaSource: audioSource{data: []byte("OggSfake"), mime: "audio/ogg"},
	}, func(_ context.Context, rep command.Reply) error { sent = rep; return nil })
	if err != nil || !handled {
		t.Fatalf("handled = %v, err = %v", handled, err)
	}
	if sent.Audio == nil || len(sent.Audio.Data) == 0 {
		t.Fatal("voice interaction must reply with a voice note when TTS is available")
	}
	if !tts.called {
		t.Fatal("TTS provider was not used")
	}
	req := text.lastRequest()
	if len(req.Messages) == 0 || req.Messages[len(req.Messages)-1].Role != "system" {
		t.Fatal("expected system prompt last")
	}
	found := false
	for _, m := range req.Messages {
		if m.Role == "user" && strings.Contains(m.Content, "hello") {
			found = true
		}
	}
	if !found {
		t.Fatal("transcript did not reach the intent engine")
	}
}

func TestRouterUncertainTranscriptionMarker(t *testing.T) {
	text := &fakeText{result: ChatResult{Text: "ok"}}
	router, _ := newTestRouter(t, &fakeModeStore{global: true}, text, &fakeSTT{res: transcript.Result{Text: "hmm hello", Uncertain: true}}, nil)
	_, _ = router.Handle(context.Background(), command.Message{
		Text: "Sonicos, what did I say?", Chat: "x@s.whatsapp.net", Sender: "1@s.whatsapp.net",
		MediaSource: audioSource{data: []byte("OggSfake"), mime: "audio/ogg"},
	}, func(context.Context, command.Reply) error { return nil })
	req := text.lastRequest()
	joined := ""
	for _, m := range req.Messages {
		joined += m.Content + "\n"
	}
	if !strings.Contains(joined, "[uncertain transcription]") {
		t.Fatal("uncertain transcription must be marked for the intent engine")
	}
}

func TestRouterNoSTTConfigured(t *testing.T) {
	router, _ := newTestRouter(t, &fakeModeStore{global: true}, &fakeText{}, nil, nil)
	var sent command.Reply
	handled, err := router.Handle(context.Background(), command.Message{
		Text: "Sonicos, explain this", Chat: "x@s.whatsapp.net", Sender: "1@s.whatsapp.net",
		MediaSource: audioSource{data: []byte("OggSfake"), mime: "audio/ogg"},
	}, func(_ context.Context, rep command.Reply) error { sent = rep; return nil })
	if err != nil || !handled {
		t.Fatalf("handled = %v, err = %v", handled, err)
	}
	if !strings.Contains(sent.Text, "Speech-to-text is not configured") {
		t.Fatalf("unexpected reply: %q", sent.Text)
	}
}

type audioSource struct {
	data []byte
	mime string
}

func (a audioSource) Download(ctx context.Context) (command.Attachment, error) {
	return command.Attachment{Kind: command.MediaAudio, MIMEType: a.mime, Data: a.data}, nil
}

func TestParseDecision(t *testing.T) {
	d := parseDecision("```json\n{\"tool\":\"sticker\",\"args\":\"\"}\n```")
	if d.Tool != "sticker" {
		t.Fatalf("fenced tool = %+v", d)
	}
	d = parseDecision("plain text answer")
	if d.Tool != "" || d.Reply != "plain text answer" {
		t.Fatalf("plain = %+v", d)
	}
}

var _ TextProvider = (*Client)(nil)
