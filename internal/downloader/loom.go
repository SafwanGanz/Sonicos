package downloader

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
)

type LoomService struct {
	*BaseService
}

func NewLoomService(client *HTTPClient) *LoomService {
	return &LoomService{
		BaseService: NewBaseService("loom", []string{
			`https?://(?:www\.)?loom\.com/share/([A-Za-z0-9]+)`,
			`https?://(?:www\.)?loom\.com/embed/([A-Za-z0-9]+)`,
		}, client),
	}
}

func (s *LoomService) Extract(ctx context.Context, urlStr string) ([]DownloadResult, error) {
	urlStr = s.cleanURL(urlStr)
	videoID := s.extractVideoID(urlStr)
	if videoID == "" {
		return nil, fmt.Errorf("could not extract video ID")
	}

	apiURL := fmt.Sprintf("https://www.loom.com/api/campaigns/sessions/%s/transcoded-urls", videoID)
	req := s.newRequest(ctx, "GET", apiURL)
	req.Header.Set("Referer", "https://www.loom.com/")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var data struct {
		URL     string `json:"url"`
		Duration int   `json:"duration"`
		Title   string `json:"title"`
		Thumbnail string `json:"thumbnail"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}

	if data.URL == "" {
		return nil, fmt.Errorf("no video URL found")
	}

	title := sanitizeFilename(data.Title)
	if title == "" {
		title = fmt.Sprintf("loom_%s", videoID)
	}

	result := DownloadResult{
		URL:        data.URL,
		Title:      title,
		MediaType:  MediaTypeVideo,
		Quality:    QualityBest,
		Extension:  "mp4",
		Duration:   data.Duration,
		Thumbnail:  data.Thumbnail,
		DirectURL:  true,
		Filename:   fmt.Sprintf("%s.mp4", title),
		Metadata: map[string]string{
			"service":   "loom",
			"video_id":  videoID,
		},
	}

	return []DownloadResult{result}, nil
}

func (s *LoomService) extractVideoID(urlStr string) string {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`/share/([A-Za-z0-9]+)`),
		regexp.MustCompile(`/embed/([A-Za-z0-9]+)`),
	}
	for _, p := range patterns {
		matches := p.FindStringSubmatch(urlStr)
		if len(matches) > 1 {
			return matches[1]
		}
	}
	return ""
}

func (s *LoomService) SupportsQuality() []Quality {
	return []Quality{QualityBest}
}

func (s *LoomService) SupportsMediaType() []MediaType {
	return []MediaType{MediaTypeVideo}
}