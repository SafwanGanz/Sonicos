package gemini

import (
	"context"

	"github.com/SafwanGanz/Sonicos/internal/transcript"
)

type Provider struct {
	stt *STT
}

func NewProvider(cfg Config) *Provider {
	stt := NewSTT(cfg)
	if stt == nil {
		return nil
	}
	return &Provider{stt: stt}
}

func (p *Provider) Name() string { return "gemini-native" }

func (p *Provider) Available() bool { return p != nil && p.stt != nil }

func (p *Provider) Transcribe(ctx context.Context, audio []byte, mime string) (transcript.Result, error) {
	if p == nil {
		return transcript.Result{}, ErrNotConfigured
	}
	text, language, uncertain, err := p.stt.Transcribe(ctx, audio, mime)
	if err != nil {
		return transcript.Result{}, err
	}
	return transcript.Result{Text: text, Language: language, Uncertain: uncertain, Provider: p.Name()}, nil
}

var ErrNotConfigured = &Error{Kind: "auth", Message: "gemini stt provider is not configured"}

func (p *Provider) HealthCheck(ctx context.Context) (string, error) {
	if p == nil {
		return "gemini-native: not configured (set GEMINI_API_KEY for free-tier audio transcription)", nil
	}
	model, err := p.stt.ResolveModel(ctx)
	if err != nil {
		return "gemini-native: configured but unavailable", err
	}
	return "gemini-native: ready (model " + model + ")", nil
}
