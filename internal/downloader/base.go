package downloader

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

type BaseService struct {
	name      string
	patterns  []*regexp.Regexp
	client    *HTTPClient
	userAgent string
	referer   string
}

func NewBaseService(name string, patterns []string, client *HTTPClient) *BaseService {
	compiled := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		compiled[i] = regexp.MustCompile(p)
	}
	return &BaseService{
		name:      name,
		patterns:  compiled,
		client:    client,
		userAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
	}
}

func (s *BaseService) Name() string               { return s.name }
func (s *BaseService) Patterns() []*regexp.Regexp { return s.patterns }

func (s *BaseService) newRequest(ctx context.Context, method, urlStr string) *http.Request {
	req, _ := http.NewRequestWithContext(ctx, method, urlStr, nil)
	req.Header.Set("User-Agent", s.userAgent)
	if s.referer != "" {
		req.Header.Set("Referer", s.referer)
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	return req
}

func (s *BaseService) extractURLParams(rawURL string) map[string]string {
	params := make(map[string]string)
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return params
	}
	for k, v := range parsed.Query() {
		if len(v) > 0 {
			params[k] = v[0]
		}
	}
	return params
}

func (s *BaseService) cleanURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	parsed.Fragment = ""
	return parsed.String()
}

func (s *BaseService) getHostname(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	hostname := strings.ToLower(parsed.Hostname())
	hostname = strings.TrimPrefix(hostname, "www.")
	hostname = strings.TrimPrefix(hostname, "m.")
	return hostname
}

func (s *BaseService) Extract(ctx context.Context, urlStr string) ([]DownloadResult, error) {
	return nil, fmt.Errorf("not implemented")
}

func (s *BaseService) SupportsQuality() []Quality {
	return []Quality{QualityBest}
}

func (s *BaseService) SupportsMediaType() []MediaType {
	return []MediaType{MediaTypeVideo, MediaTypeAudio}
}
