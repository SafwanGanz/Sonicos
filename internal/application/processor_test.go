package application

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/SafwanGanz/Sonicos/internal/command"
	"github.com/SafwanGanz/Sonicos/internal/idempotency"
)

type recordingRouter struct {
	calls int
}

func (r *recordingRouter) Handle(context.Context, command.Message, Reply) (bool, error) {
	r.calls++
	return true, nil
}

func newTestProcessor(t *testing.T, router AIRouter) *Processor {
	t.Helper()
	registry := command.NewRegistry()
	if err := registry.Register(command.Command{
		Name: "ping",
		Handler: func(_ context.Context, _ command.Message, _ []string, reply Reply) error {
			return reply(context.Background(), command.Reply{Text: "pong"})
		},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proc, err := NewProcessor(registry, idempotency.NewMemory(), "!", 5*time.Second, logger)
	if err != nil {
		t.Fatalf("NewProcessor: %v", err)
	}
	proc.SetAIRouter(router)
	return proc
}

func TestPrefixCommandsBypassAI(t *testing.T) {
	router := &recordingRouter{}
	proc := newTestProcessor(t, router)
	var replies []string
	err := proc.Handle(context.Background(), command.Message{
		ID: "m1", Chat: "x@g.us", Sender: "1@s.whatsapp.net", Text: "!ping", Group: true,
	}, func(_ context.Context, rep command.Reply) error {
		replies = append(replies, rep.Text)
		return nil
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if router.calls != 0 {
		t.Fatalf("AI router must not run for prefix commands, calls = %d", router.calls)
	}
	if len(replies) != 1 || replies[0] != "pong" {
		t.Fatalf("legacy replies = %v", replies)
	}
}

func TestNaturalLanguageGoesToAI(t *testing.T) {
	router := &recordingRouter{}
	proc := newTestProcessor(t, router)
	err := proc.Handle(context.Background(), command.Message{
		ID: "m2", Chat: "x@g.us", Sender: "1@s.whatsapp.net", Text: "Sonicos, hello", Group: true,
	}, func(context.Context, command.Reply) error { return nil })
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if router.calls != 1 {
		t.Fatalf("AI router calls = %d, want 1", router.calls)
	}
}

func TestNoRouterNoOp(t *testing.T) {
	proc := newTestProcessor(t, nil)
	err := proc.Handle(context.Background(), command.Message{
		ID: "m3", Chat: "x@g.us", Sender: "1@s.whatsapp.net", Text: "Sonicos, hello", Group: true,
	}, func(context.Context, command.Reply) error { return nil })
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
}
