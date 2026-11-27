package downloader

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type MediaType string

const (
	MediaTypeVideo  MediaType = "video"
	MediaTypeAudio  MediaType = "audio"
	MediaTypeImage  MediaType = "image"
	MediaTypeMixed  MediaType = "mixed"
)

type Quality string

const (
	QualityBest   Quality = "best"
	Quality1080p  Quality = "1080p"
	Quality720p   Quality = "720p"
	Quality480p   Quality = "480p"
	Quality360p   Quality = "360p"
	QualityAudio  Quality = "audio"
)

type DownloadResult struct {
	URL         string            `json:"url"`
	MediaType   MediaType         `json:"media_type"`
	Quality     Quality           `json:"quality"`
	Extension   string            `json:"extension"`
	Filename    string            `json:"filename"`
	Filesize    int64             `json:"filesize,omitempty"`
	Duration    int               `json:"duration,omitempty"`
	Title       string            `json:"title,omitempty"`
	Thumbnail   string            `json:"thumbnail,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	DirectURL   bool              `json:"direct_url"`
}

type Service interface {
	Name() string
	Patterns() []*regexp.Regexp
	Extract(ctx context.Context, urlStr string) ([]DownloadResult, error)
	SupportsQuality() []Quality
	SupportsMediaType() []MediaType
}

type Downloader struct {
	services []Service
	client   *HTTPClient
	timeout  time.Duration
}

func NewDownloader(timeout time.Duration) *Downloader {
	d := &Downloader{
		client:  NewHTTPClient(timeout),
		timeout: timeout,
	}
	d.registerServices()
	return d
}

func (d *Downloader) registerServices() {
	d.services = []Service{
		NewInstagramService(d.client),
		NewYouTubeService(d.client),
		NewTikTokService(d.client),
		NewTwitterService(d.client),
		NewRedditService(d.client),
		NewVimeoService(d.client),
		NewSoundCloudService(d.client),
		NewPinterestService(d.client),
		NewFacebookService(d.client),
		NewTwitchService(d.client),
		NewStreamableService(d.client),
		NewDailymotionService(d.client),
		NewBilibiliService(d.client),
		NewBlueskyService(d.client),
		NewTumblrService(d.client),
		NewLoomService(d.client),
		NewNewgroundsService(d.client),
		NewOKService(d.client),
		NewRutubeService(d.client),
		NewSnapchatService(d.client),
		NewVKService(d.client),
	}
}

func (d *Downloader) Extract(ctx context.Context, urlStr string) ([]DownloadResult, error) {
	urlStr = strings.TrimSpace(urlStr)
	if urlStr == "" {
		return nil, fmt.Errorf("empty URL")
	}

	parsed, err := url.Parse(urlStr)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}

	hostname := strings.ToLower(parsed.Hostname())
	hostname = strings.TrimPrefix(hostname, "www.")
	hostname = strings.TrimPrefix(hostname, "m.")

	for _, svc := range d.services {
		for _, pattern := range svc.Patterns() {
			if pattern.MatchString(urlStr) {
				results, err := svc.Extract(ctx, urlStr)
				if err != nil {
					return nil, fmt.Errorf("%s extraction failed: %w", svc.Name(), err)
				}
				return results, nil
			}
		}
	}

	return nil, fmt.Errorf("unsupported URL: %s", hostname)
}

func (d *Downloader) GetSupportedServices() []string {
	names := make([]string, len(d.services))
	for i, svc := range d.services {
		names[i] = svc.Name()
	}
	return names
}

func (d *Downloader) Close() error {
	return d.client.Close()
}

func (d *Downloader) Client() *HTTPClient {
	return d.client
}

func (d *Downloader) NewRequest(ctx context.Context, method, urlStr string) *http.Request {
	req, _ := http.NewRequestWithContext(ctx, method, urlStr, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	return req
}