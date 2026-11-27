package ai

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLiveWeb2API(t *testing.T) {
	if os.Getenv("SONICOS_LIVE_WEB2API") != "1" {
		t.Skip("set SONICOS_LIVE_WEB2API=1 to run against the local server")
	}
	base := envOr("GEMINI_WEB2API_BASE_URL", "http://127.0.0.1:8081/v1")
	key := os.Getenv("GEMINI_WEB2API_API_KEY")
	c := NewClient(Config{APIKey: key, BaseURL: base, Model: os.Getenv("GEMINI_WEB2API_MODEL"), Timeout: 60 * time.Second, MaxRetries: 1, AllowAnonymous: true})
	if c == nil {
		t.Fatal("client not configured")
	}
	ctx := context.Background()

	ids, err := c.Models(ctx)
	if err != nil {
		t.Fatalf("model discovery failed (is the server running?): %v", err)
	}
	t.Logf("discovered models: %s", strings.Join(ids, ", "))

	model, err := c.ResolveModel(ctx, os.Getenv("GEMINI_WEB2API_MODEL"), envOr("GEMINI_WEB2API_FALLBACK_MODEL", "gemini-3.5-flash"))
	if err != nil {
		t.Fatalf("model validation failed: %v", err)
	}
	t.Logf("resolved model: %s", model)

	res, err := c.Chat(ctx, ChatRequest{Messages: []Message{{Role: "user", Content: "Reply with exactly: pong"}}})
	if err != nil {
		t.Fatalf("chat failed: %v", err)
	}
	if !strings.Contains(strings.ToLower(res.Text), "pong") {
		t.Fatalf("unexpected chat reply: %q", res.Text)
	}

	res, err = c.Chat(ctx, ChatRequest{
		Messages: []Message{{Role: "user", Content: "Create a sticker from the quoted image"}},
		Tools:    []ToolSchema{{Name: "sticker", Description: "Turn an image into a sticker"}},
	})
	if err != nil {
		t.Fatalf("tool chat failed: %v", err)
	}
	if len(res.ToolCalls) == 0 && !strings.Contains(res.Text, "sticker") {
		t.Fatalf("tool call not surfaced: %+v", res)
	}
	t.Logf("tool result: %+v", res)

	if key != "" {
		bad := NewClient(Config{APIKey: key + "-wrong", BaseURL: base, Model: model, Timeout: 30 * time.Second, AllowAnonymous: true})
		if _, err := bad.Chat(ctx, ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}}); err == nil {
			t.Fatal("wrong API key must be rejected")
		}
	}
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
