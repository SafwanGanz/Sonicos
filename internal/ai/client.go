package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/SafwanGanz/Sonicos/internal/transcript"
)

type Config struct {
	APIKey     string
	BaseURL    string
	Model      string
	Timeout    time.Duration
	MaxRetries int

	AllowAnonymous bool
}

type Message struct {
	Role    string
	Content string
}

type ToolSchema struct {
	Name        string
	Description string
	Parameters  map[string]any
}

type ToolCall struct {
	Name      string
	Arguments string
}

type ChatRequest struct {
	Messages []Message
	Tools    []ToolSchema
}

type ChatResult struct {
	Text      string
	ToolCalls []ToolCall
}

type TextProvider interface {
	Name() string
	Available() bool
	Chat(ctx context.Context, req ChatRequest) (ChatResult, error)
	ChatStream(ctx context.Context, req ChatRequest, onDelta func(string)) (ChatResult, error)
}

type Client struct {
	cfg  Config
	http *http.Client

	mu       sync.RWMutex
	models   []string
	loadedAt time.Time
	resolved string
}

const modelCacheTTL = time.Hour

func NewClient(cfg Config) *Client {
	if strings.TrimSpace(cfg.APIKey) == "" && !cfg.AllowAnonymous {
		return nil
	}
	if strings.TrimSpace(cfg.BaseURL) == "" {
		cfg.BaseURL = "https://api.openai.com/v1"
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.Timeout <= 0 {
		cfg.Timeout = 180 * time.Second
	}
	if cfg.MaxRetries < 0 {
		cfg.MaxRetries = 0
	}
	return &Client{cfg: cfg, http: &http.Client{Timeout: cfg.Timeout}}
}

func (c *Client) Name() string { return "openai-compatible" }

func (c *Client) Available() bool { return c != nil }

func (c *Client) Models(ctx context.Context) ([]string, error) {
	if c == nil {
		return nil, fmt.Errorf("ai backend is not configured")
	}
	c.mu.RLock()
	if len(c.models) > 0 && time.Since(c.loadedAt) < modelCacheTTL {
		out := append([]string(nil), c.models...)
		c.mu.RUnlock()
		return out, nil
	}
	c.mu.RUnlock()

	raw, err := c.do(ctx, http.MethodGet, "/models", nil, 0)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode models response: %w", err)
	}
	ids := make([]string, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("no models discovered")
	}
	c.mu.Lock()
	c.models = ids
	c.loadedAt = time.Now()
	c.mu.Unlock()
	return append([]string(nil), ids...), nil
}

func (c *Client) ResolveModel(ctx context.Context, preferred, fallback string) (string, error) {
	if c == nil {
		return "", fmt.Errorf("ai backend is not configured")
	}
	c.mu.RLock()
	resolved := c.resolved
	c.mu.RUnlock()
	if resolved != "" {
		return resolved, nil
	}
	ids, err := c.Models(ctx)
	if err != nil {
		return "", err
	}
	available := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		available[id] = struct{}{}
	}
	for _, want := range []string{preferred, fallback} {
		want = strings.TrimSpace(want)
		if want == "" {
			continue
		}
		if _, ok := available[want]; ok {
			c.mu.Lock()
			c.resolved = want
			c.mu.Unlock()
			return want, nil
		}
	}
	return "", fmt.Errorf("configured model %q and fallback %q are unavailable; discovered models: %s", preferred, fallback, strings.Join(ids, ", "))
}

type chatRequestWire struct {
	Model       string    `json:"model"`
	Messages    []chatMsg `json:"messages"`
	Temperature float64   `json:"temperature"`
	Tools       []any     `json:"tools,omitempty"`
	Stream      bool      `json:"stream,omitempty"`
}

type chatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
}

func (c *Client) buildBody(req ChatRequest) (chatRequestWire, error) {
	model := c.cfg.Model
	if resolved := c.modelID(); resolved != "" {
		model = resolved
	}
	if strings.TrimSpace(model) == "" {
		return chatRequestWire{}, fmt.Errorf("no model configured")
	}
	body := chatRequestWire{Model: model, Temperature: 0.2}
	for _, m := range req.Messages {
		body.Messages = append(body.Messages, chatMsg{Role: m.Role, Content: m.Content})
	}
	for _, t := range req.Tools {
		params := t.Parameters
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		body.Tools = append(body.Tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  params,
			},
		})
	}
	return body, nil
}

func (c *Client) modelID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.resolved
}

func (c *Client) Chat(ctx context.Context, req ChatRequest) (ChatResult, error) {
	body, err := c.buildBody(req)
	if err != nil {
		return ChatResult{}, err
	}
	raw, err := c.do(ctx, http.MethodPost, "/chat/completions", body, c.cfg.MaxRetries)
	if err != nil {
		return ChatResult{}, err
	}
	var parsed chatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ChatResult{}, fmt.Errorf("decode chat response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return ChatResult{}, fmt.Errorf("chat response has no choices")
	}
	msg := parsed.Choices[0].Message
	res := ChatResult{Text: strings.TrimSpace(msg.Content)}
	for _, tc := range msg.ToolCalls {
		res.ToolCalls = append(res.ToolCalls, ToolCall{Name: tc.Function.Name, Arguments: tc.Function.Arguments})
	}
	return res, nil
}

func (c *Client) ChatStream(ctx context.Context, req ChatRequest, onDelta func(string)) (ChatResult, error) {
	body, err := c.buildBody(req)
	if err != nil {
		return ChatResult{}, err
	}
	body.Stream = true
	encoded, err := json.Marshal(body)
	if err != nil {
		return ChatResult{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/chat/completions", bytes.NewReader(encoded))
	if err != nil {
		return ChatResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return ChatResult{}, fmt.Errorf("call chat completions: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return ChatResult{}, fmt.Errorf("chat completions HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var result ChatResult
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk chatResponse
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		if content := chunk.Choices[0].Message.Content; content != "" {
			result.Text += content
			if onDelta != nil {
				onDelta(content)
			}
		}
		for _, tc := range chunk.Choices[0].Message.ToolCalls {
			result.ToolCalls = append(result.ToolCalls, ToolCall{Name: tc.Function.Name, Arguments: tc.Function.Arguments})
		}
	}
	return result, nil
}

func (c *Client) do(ctx context.Context, method, path string, payload any, retries int) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		var bodyReader io.Reader
		if payload != nil {
			encoded, err := json.Marshal(payload)
			if err != nil {
				return nil, fmt.Errorf("encode request: %w", err)
			}
			bodyReader = bytes.NewReader(encoded)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.cfg.BaseURL+path, bodyReader)
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if c.cfg.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("call %s: %w", path, err)
			if attempt < retries {
				continue
			}
			return nil, lastErr
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("read %s response: %w", path, readErr)
			if attempt < retries {
				continue
			}
			return nil, lastErr
		}
		if resp.StatusCode == http.StatusOK {
			return raw, nil
		}
		lastErr = fmt.Errorf("%s HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(raw)))
		if (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500) && attempt < retries {
			continue
		}
		return nil, lastErr
	}
	return nil, lastErr
}

type OpenAITranscription struct {
	client *Client
	model  string
}

func NewOpenAITranscription(client *Client, model string) *OpenAITranscription {
	if client == nil {
		return nil
	}
	if strings.TrimSpace(model) == "" {
		model = "whisper-1"
	}
	return &OpenAITranscription{client: client, model: model}
}

func (t *OpenAITranscription) Name() string { return "openai-compatible-stt" }

func (t *OpenAITranscription) Available() bool { return t != nil && t.client != nil }

func (t *OpenAITranscription) Transcribe(ctx context.Context, audio []byte, mime string) (transcript.Result, error) {
	if t == nil {
		return transcript.Result{}, fmt.Errorf("transcription provider is not configured")
	}
	if len(audio) == 0 {
		return transcript.Result{}, fmt.Errorf("empty audio payload")
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("model", t.model); err != nil {
		return transcript.Result{}, err
	}
	if mime == "" {
		mime = "application/octet-stream"
	}
	part, err := w.CreateFormFile("file", "audio"+extensionForMIME(mime))
	if err != nil {
		return transcript.Result{}, err
	}
	if _, err := part.Write(audio); err != nil {
		return transcript.Result{}, err
	}
	if err := w.Close(); err != nil {
		return transcript.Result{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, t.client.cfg.BaseURL+"/audio/transcriptions", &buf)
	if err != nil {
		return transcript.Result{}, err
	}
	httpReq.Header.Set("Content-Type", w.FormDataContentType())
	httpReq.Header.Set("Authorization", "Bearer "+t.client.cfg.APIKey)
	resp, err := t.client.http.Do(httpReq)
	if err != nil {
		return transcript.Result{}, fmt.Errorf("call transcriptions: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return transcript.Result{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return transcript.Result{}, fmt.Errorf("transcriptions HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var parsed struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return transcript.Result{}, fmt.Errorf("decode transcription response: %w", err)
	}
	return transcript.Result{Text: strings.TrimSpace(parsed.Text)}, nil
}

func extensionForMIME(mime string) string {
	switch {
	case strings.Contains(mime, "ogg"):
		return ".ogg"
	case strings.Contains(mime, "mp4"), strings.Contains(mime, "m4a"):
		return ".m4a"
	case strings.Contains(mime, "mpeg"), strings.Contains(mime, "mp3"):
		return ".mp3"
	case strings.Contains(mime, "wav"):
		return ".wav"
	default:
		return ".webm"
	}
}
