package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c := NewClient(Config{APIKey: "k", BaseURL: srv.URL, Model: "m1", Timeout: 5 * time.Second, MaxRetries: 2})
	if c == nil {
		t.Fatal("NewClient returned nil")
	}
	return c
}

func TestModelsDiscoveryAndCache(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			atomic.AddInt32(&hits, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
				map[string]any{"id": "gemini-3.5-flash"},
				map[string]any{"id": "gemini-3.6-flash"},
			}})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := newTestClient(t, srv)
	ctx := context.Background()
	ids, err := c.Models(ctx)
	if err != nil || len(ids) != 2 {
		t.Fatalf("Models = %v, err = %v", ids, err)
	}
	if _, err := c.Models(ctx); err != nil {
		t.Fatalf("cached Models: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("models endpoint hits = %d, want 1 (cached)", got)
	}
}

func TestResolveModelFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
			map[string]any{"id": "gemini-3.6-flash"},
		}})
	}))
	defer srv.Close()
	c := newTestClient(t, srv)
	got, err := c.ResolveModel(context.Background(), "missing-model", "gemini-3.6-flash")
	if err != nil || got != "gemini-3.6-flash" {
		t.Fatalf("ResolveModel = %q, err = %v", got, err)
	}
}

func TestResolveModelNeverSilent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
			map[string]any{"id": "unrelated-model"},
		}})
	}))
	defer srv.Close()
	c := newTestClient(t, srv)
	_, err := c.ResolveModel(context.Background(), "wanted", "fallback")
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("expected unavailable error, got %v", err)
	}
}

func TestChatSendsHistoryAndTools(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&captured)
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{
			map[string]any{"message": map[string]any{"role": "assistant", "content": "pong"}},
		}})
	}))
	defer srv.Close()
	c := newTestClient(t, srv)
	res, err := c.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "hello"}, {Role: "user", Content: "ping"}},
		Tools:    []ToolSchema{{Name: "echo", Description: "Echo"}},
	})
	if err != nil || res.Text != "pong" {
		t.Fatalf("Chat = %+v, err = %v", res, err)
	}
	msgs, _ := captured["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("history not sent verbatim: %v", captured["messages"])
	}
	tools, _ := captured["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools not sent: %v", captured["tools"])
	}
}

func TestChatRetriesOnServerError(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{
			map[string]any{"message": map[string]any{"content": "ok"}},
		}})
	}))
	defer srv.Close()
	c := newTestClient(t, srv)
	res, err := c.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil || res.Text != "ok" {
		t.Fatalf("Chat = %+v, err = %v", res, err)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Fatalf("hits = %d, want 3 (two retries)", got)
	}
}

func TestChatNoRetryOnAuthError(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := newTestClient(t, srv)
	if _, err := c.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}}); err == nil {
		t.Fatal("expected auth error")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("auth errors must not be retried, hits = %d", got)
	}
}

func TestChatStreamAggregates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"message\":{\"content\":\"Hel\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"message\":{\"content\":\"lo\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()
	c := newTestClient(t, srv)
	var deltas []string
	res, err := c.ChatStream(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}}, func(d string) { deltas = append(deltas, d) })
	if err != nil || res.Text != "Hello" {
		t.Fatalf("ChatStream = %+v, err = %v", res, err)
	}
	if strings.Join(deltas, "") != "Hello" {
		t.Fatalf("deltas = %v", deltas)
	}
}

func TestAnonymousClientAllowed(t *testing.T) {
	c := NewClient(Config{BaseURL: "http://127.0.0.1:8081/v1", AllowAnonymous: true})
	if c == nil || !c.Available() {
		t.Fatal("anonymous client must be available for auth-disabled servers")
	}
	if NewClient(Config{BaseURL: "http://x"}) != nil {
		t.Fatal("empty key without AllowAnonymous must stay nil")
	}
}

func TestToolCallsParsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{
			map[string]any{"message": map[string]any{"content": "", "tool_calls": []any{
				map[string]any{"function": map[string]any{"name": "sticker", "arguments": "{}"}},
			}}},
		}})
	}))
	defer srv.Close()
	c := newTestClient(t, srv)
	res, err := c.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil || len(res.ToolCalls) != 1 || res.ToolCalls[0].Name != "sticker" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
}
