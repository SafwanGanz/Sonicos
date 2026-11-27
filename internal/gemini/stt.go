package gemini

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Config struct {
	APIKey        string
	BaseURL       string
	Model         string
	FallbackModel string
	Timeout       time.Duration
	MaxRetries    int
}

type STT struct {
	cfg  Config
	http *http.Client

	mu        sync.RWMutex
	models    []string
	loadedAt  time.Time
	resolved  string
	authError bool
}

const modelCacheTTL = time.Hour

func NewSTT(cfg Config) *STT {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil
	}
	if strings.TrimSpace(cfg.BaseURL) == "" {
		cfg.BaseURL = "https://generativelanguage.googleapis.com/v1beta"
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.Timeout <= 0 {
		cfg.Timeout = 120 * time.Second
	}
	if cfg.MaxRetries < 0 {
		cfg.MaxRetries = 0
	}
	return &STT{cfg: cfg, http: &http.Client{Timeout: cfg.Timeout}}
}

func (s *STT) Name() string { return "gemini-native" }

func (s *STT) Available() bool { return s != nil }

type listModelsResponse struct {
	Models []struct {
		Name                       string   `json:"name"`
		SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
	} `json:"models"`
}

type apiError struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

type Error struct {
	Kind    string
	Status  int
	Message string
}

func (e *Error) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("%s (HTTP %d): %s", e.Kind, e.Status, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Kind, e.Message)
}

func (s *STT) classify(status int, body []byte) *Error {
	msg := strings.TrimSpace(string(body))
	var parsed apiError
	if json.Unmarshal(body, &parsed) == nil && parsed.Error.Message != "" {
		msg = parsed.Error.Message
	}
	kind := "upstream"
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		kind = "auth"
	case http.StatusNotFound:
		kind = "model_unavailable"
	case http.StatusTooManyRequests:
		kind = "rate_limit"
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		kind = "timeout"
	}
	return &Error{Kind: kind, Status: status, Message: msg}
}

func (s *STT) Models(ctx context.Context) ([]string, error) {
	s.mu.RLock()
	if len(s.models) > 0 && time.Since(s.loadedAt) < modelCacheTTL {
		out := append([]string(nil), s.models...)
		s.mu.RUnlock()
		return out, nil
	}
	s.mu.RUnlock()

	body, err := s.do(ctx, http.MethodGet, "/models?pageSize=1000", nil, 0)
	if err != nil {
		return nil, err
	}
	var parsed listModelsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, &Error{Kind: "upstream", Message: "decode models response"}
	}
	ids := make([]string, 0, len(parsed.Models))
	for _, m := range parsed.Models {
		supports := false
		for _, method := range m.SupportedGenerationMethods {
			if method == "generateContent" {
				supports = true
				break
			}
		}
		if !supports {
			continue
		}
		id := strings.TrimPrefix(m.Name, "models/")
		if id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, &Error{Kind: "model_unavailable", Message: "no generateContent models discovered"}
	}
	s.mu.Lock()
	s.models = ids
	s.loadedAt = time.Now()
	s.mu.Unlock()
	return append([]string(nil), ids...), nil
}

func (s *STT) ResolveModel(ctx context.Context) (string, error) {
	s.mu.RLock()
	resolved := s.resolved
	s.mu.RUnlock()
	if resolved != "" {
		return resolved, nil
	}
	ids, err := s.Models(ctx)
	if err != nil {
		return "", err
	}
	available := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		available[id] = struct{}{}
	}
	candidates := []string{s.cfg.Model, s.cfg.FallbackModel}
	for _, want := range candidates {
		want = strings.TrimSpace(want)
		if want == "" {
			continue
		}
		if _, ok := available[want]; ok {
			s.mu.Lock()
			s.resolved = want
			s.mu.Unlock()
			return want, nil
		}
	}
	return "", &Error{Kind: "model_unavailable", Message: fmt.Sprintf("configured model %q and fallback %q are unavailable; discovered models: %s", s.cfg.Model, s.cfg.FallbackModel, strings.Join(ids, ", "))}
}

func (s *STT) do(ctx context.Context, method, path string, payload any, retries int) ([]byte, error) {
	var bodyReader io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, &Error{Kind: "upstream", Message: "encode request"}
		}
		bodyReader = strings.NewReader(string(encoded))
	}
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, s.cfg.BaseURL+path, bodyReader)
		if err != nil {
			return nil, &Error{Kind: "upstream", Message: "build request"}
		}
		req.Header.Set("x-goog-api-key", s.cfg.APIKey)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := s.http.Do(req)
		if err != nil {
			lastErr = &Error{Kind: "network", Message: err.Error()}
			if attempt < retries {
				continue
			}
			return nil, lastErr
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if readErr != nil {
			lastErr = &Error{Kind: "network", Message: "read response"}
			if attempt < retries {
				continue
			}
			return nil, lastErr
		}
		if resp.StatusCode == http.StatusOK {
			return raw, nil
		}
		classified := s.classify(resp.StatusCode, raw)
		if classified.Kind == "rate_limit" || classified.Status >= 500 {
			lastErr = classified
			if attempt < retries {
				continue
			}
			return nil, lastErr
		}
		return nil, classified
	}
	return nil, lastErr
}

type inlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type part struct {
	InlineData inlineData `json:"inlineData,omitempty"`
	Text       string     `json:"text,omitempty"`
}

type content struct {
	Role  string `json:"role,omitempty"`
	Parts []part `json:"parts"`
}

type generateRequest struct {
	Contents         []content `json:"contents"`
	GenerationConfig struct {
		Temperature float64 `json:"temperature"`
	} `json:"generationConfig"`
}

type generateResponse struct {
	Candidates []struct {
		Content content `json:"content"`
	} `json:"candidates"`
}

const transcriptionPrompt = `Transcribe the attached audio exactly as spoken.
Rules:
- Preserve the original language and wording verbatim; do NOT translate.
- Keep slang, colloquialisms, and filler words.
- Handle mixed-language speech as heard.
- If parts are unclear, mark them inline with [?] instead of inventing words.
- If the audio is unusable, respond with exactly: {"error":"unusable"}
Respond with ONLY a JSON object: {"language": "<BCP-47-ish language name or code you detected>", "uncertain": true|false, "text": "<verbatim transcript>"}`

func (s *STT) Transcribe(ctx context.Context, audio []byte, mime string) (text string, language string, uncertain bool, err error) {
	if s == nil {
		return "", "", false, errors.New("gemini stt provider is not configured")
	}
	if len(audio) == 0 {
		return "", "", false, &Error{Kind: "invalid_audio", Message: "empty audio payload"}
	}
	if mime == "" {
		mime = "audio/ogg"
	}
	model, err := s.ResolveModel(ctx)
	if err != nil {
		return "", "", false, err
	}
	payload := generateRequest{
		Contents: []content{{
			Role: "user",
			Parts: []part{
				{InlineData: inlineData{MimeType: mime, Data: base64.StdEncoding.EncodeToString(audio)}},
				{Text: transcriptionPrompt},
			},
		}},
	}
	raw, err := s.do(ctx, http.MethodPost, "/models/"+model+":generateContent", payload, s.cfg.MaxRetries)
	if err != nil {
		return "", "", false, err
	}
	var parsed generateResponse
	if err := json.Unmarshal(raw, &parsed); err != nil || len(parsed.Candidates) == 0 {
		return "", "", false, &Error{Kind: "upstream", Message: "no transcription candidates in response"}
	}
	var sb strings.Builder
	for _, p := range parsed.Candidates[0].Content.Parts {
		sb.WriteString(p.Text)
	}
	out := strings.TrimSpace(sb.String())
	if out == "" {
		return "", "", false, &Error{Kind: "upstream", Message: "empty transcription"}
	}
	if strings.TrimSpace(out) == `{"error":"unusable"}` {
		return "", "", false, &Error{Kind: "invalid_audio", Message: "audio is unusable"}
	}

	var structured struct {
		Language  string `json:"language"`
		Uncertain bool   `json:"uncertain"`
		Text      string `json:"text"`
	}
	if json.Unmarshal([]byte(out), &structured) == nil && structured.Text != "" {
		return strings.TrimSpace(structured.Text), strings.TrimSpace(structured.Language), structured.Uncertain, nil
	}
	return out, "", false, nil
}
