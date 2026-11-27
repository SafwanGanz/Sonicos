package gemini

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestSTT(t *testing.T, srv *httptest.Server) *STT {
	t.Helper()
	s := NewSTT(Config{APIKey: "test-key", BaseURL: srv.URL, Model: "gemini-2.5-flash", FallbackModel: "gemini-flash-lite", Timeout: 5 * time.Second, MaxRetries: 1})
	if s == nil {
		t.Fatal("NewSTT returned nil")
	}
	return s
}

func TestNilWithoutKey(t *testing.T) {
	if NewSTT(Config{}) != nil {
		t.Fatal("provider must be nil without an API key")
	}
}

func TestModelsDiscoveryFiltersGenerateContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{
			map[string]any{"name": "models/gemini-2.5-flash", "supportedGenerationMethods": []string{"generateContent", "embedContent"}},
			map[string]any{"name": "models/text-embedding-004", "supportedGenerationMethods": []string{"embedContent"}},
		}})
	}))
	defer srv.Close()
	s := newTestSTT(t, srv)
	ids, err := s.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(ids) != 1 || ids[0] != "gemini-2.5-flash" {
		t.Fatalf("ids = %v", ids)
	}
}

func TestResolveModelConfiguredWins(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{
			map[string]any{"name": "models/gemini-2.5-flash", "supportedGenerationMethods": []string{"generateContent"}},
			map[string]any{"name": "models/gemini-flash-lite", "supportedGenerationMethods": []string{"generateContent"}},
		}})
	}))
	defer srv.Close()
	s := newTestSTT(t, srv)
	got, err := s.ResolveModel(context.Background())
	if err != nil || got != "gemini-2.5-flash" {
		t.Fatalf("ResolveModel = %q, err = %v", got, err)
	}
}

func TestResolveModelFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{
			map[string]any{"name": "models/gemini-flash-lite", "supportedGenerationMethods": []string{"generateContent"}},
		}})
	}))
	defer srv.Close()
	s := newTestSTT(t, srv)
	got, err := s.ResolveModel(context.Background())
	if err != nil || got != "gemini-flash-lite" {
		t.Fatalf("ResolveModel = %q, err = %v", got, err)
	}
}

func TestTranscribeSendsInlineAudio(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{
				map[string]any{"name": "models/gemini-2.5-flash", "supportedGenerationMethods": []string{"generateContent"}},
			}})
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&captured)
		_ = json.NewEncoder(w).Encode(map[string]any{"candidates": []any{
			map[string]any{"content": map[string]any{"parts": []any{
				map[string]any{"text": `{"language":"English","uncertain":false,"text":"hello world"}`},
			}}},
		}})
	}))
	defer srv.Close()
	s := newTestSTT(t, srv)
	text, lang, uncertain, err := s.Transcribe(context.Background(), []byte("OggS-audio"), "audio/ogg")
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if text != "hello world" || lang != "English" || uncertain {
		t.Fatalf("result = %q %q %v", text, lang, uncertain)
	}
	contents, _ := captured["contents"].([]any)
	if len(contents) != 1 {
		t.Fatalf("contents = %v", captured)
	}
	parts, _ := contents[0].(map[string]any)["parts"].([]any)
	inline, _ := parts[0].(map[string]any)["inlineData"].(map[string]any)
	if inline["mimeType"] != "audio/ogg" {
		t.Fatalf("mime = %v", inline["mimeType"])
	}
	decoded, err := base64.StdEncoding.DecodeString(inline["data"].(string))
	if err != nil || string(decoded) != "OggS-audio" {
		t.Fatalf("audio payload mismatch: %v", err)
	}
}

func TestTranscribeLenientPlainText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{
				map[string]any{"name": "models/gemini-2.5-flash", "supportedGenerationMethods": []string{"generateContent"}},
			}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"candidates": []any{
			map[string]any{"content": map[string]any{"parts": []any{map[string]any{"text": "plain transcript"}}}},
		}})
	}))
	defer srv.Close()
	s := newTestSTT(t, srv)
	text, _, _, err := s.Transcribe(context.Background(), []byte("audio"), "")
	if err != nil || text != "plain transcript" {
		t.Fatalf("text = %q, err = %v", text, err)
	}
}

func TestTranscribeUnusableAudio(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{
				map[string]any{"name": "models/gemini-2.5-flash", "supportedGenerationMethods": []string{"generateContent"}},
			}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"candidates": []any{
			map[string]any{"content": map[string]any{"parts": []any{map[string]any{"text": `{"error":"unusable"}`}}}},
		}})
	}))
	defer srv.Close()
	s := newTestSTT(t, srv)
	_, _, _, err := s.Transcribe(context.Background(), []byte("audio"), "audio/ogg")
	var apiErr *Error
	if err == nil || !asError(err, &apiErr) || apiErr.Kind != "invalid_audio" {
		t.Fatalf("expected invalid_audio error, got %v", err)
	}
}

func TestErrorClassification(t *testing.T) {
	cases := []struct {
		status int
		kind   string
	}{
		{http.StatusUnauthorized, "auth"},
		{http.StatusNotFound, "model_unavailable"},
		{http.StatusTooManyRequests, "rate_limit"},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
		}))
		s := newTestSTT(t, srv)
		_, _, _, err := s.Transcribe(context.Background(), []byte("audio"), "audio/ogg")
		var apiErr *Error
		if err == nil || !asError(err, &apiErr) || apiErr.Kind != tc.kind {
			t.Fatalf("status %d: expected kind %s, got %v", tc.status, tc.kind, err)
		}
		srv.Close()
	}
}

func asError(err error, target **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*target = e
	}
	return ok
}

func TestTranscribeEmptyAudio(t *testing.T) {
	s := NewSTT(Config{APIKey: "k", BaseURL: "http://unused"})
	_, _, _, err := s.Transcribe(context.Background(), nil, "audio/ogg")
	if err == nil || !strings.Contains(err.Error(), "empty audio") {
		t.Fatalf("err = %v", err)
	}
}
