package downloader

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
)

type OKService struct {
	*BaseService
}

func NewOKService(client *HTTPClient) *OKService {
	return &OKService{
		BaseService: NewBaseService("ok", []string{
			`https?://(?:www\.)?ok\.ru/video/(\d+)`,
			`https?://(?:www\.)?ok\.ru/live/(\d+)`,
		}, client),
	}
}

func (s *OKService) Extract(ctx context.Context, urlStr string) ([]DownloadResult, error) {
	urlStr = s.cleanURL(urlStr)
	videoID := s.extractVideoID(urlStr)
	if videoID == "" {
		return nil, fmt.Errorf("could not extract video ID")
	}

	apiURL := fmt.Sprintf("https://ok.ru/video/%s", videoID)
	req := s.newRequest(ctx, "GET", apiURL)
	req.Header.Set("Referer", "https://ok.ru/")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	metadata := s.extractMetadata(bodyStr)
	if metadata == nil {
		return nil, fmt.Errorf("could not extract video metadata")
	}

	title := sanitizeFilename(metadata.Title)
	if title == "" {
		title = fmt.Sprintf("ok_%s", videoID)
	}

	var results []DownloadResult
	for _, v := range metadata.Videos {
		if v.URL == "" {
			continue
		}
		result := DownloadResult{
			URL:        v.URL,
			Title:      title,
			MediaType:  MediaTypeVideo,
			Quality:    s.parseQuality(v.Name),
			Extension:  "mp4",
			Duration:   metadata.Duration,
			Thumbnail:  metadata.Poster,
			DirectURL:  true,
			Filename:   fmt.Sprintf("%s_%s.mp4", title, v.Name),
			Metadata: map[string]string{
				"service":   "ok",
				"video_id":  videoID,
				"quality":   v.Name,
			},
		}
		results = append(results, result)
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("no video sources found")
	}

	return results, nil
}

type okMetadata struct {
	Title    string
	Duration int
	Poster   string
	Videos   []struct {
		Name string
		URL  string
	}
}

func (s *OKService) extractMetadata(html string) *okMetadata {
	metadata := &okMetadata{}
	
	patterns := map[string]*regexp.Regexp{
		"title":      regexp.MustCompile(`<meta property="og:title" content="([^"]+)"`),
		"duration":   regexp.MustCompile(`<meta property="og:video:duration" content="(\d+)"`),
		"poster":     regexp.MustCompile(`<meta property="og:image" content="([^"]+)"`),
		"videos":     regexp.MustCompile(`"url":"([^"]+)"[^}]*"name":"([^"]+)"`),
	}

	if m := patterns["title"].FindStringSubmatch(html); len(m) > 1 {
		metadata.Title = m[1]
	}
	if m := patterns["duration"].FindStringSubmatch(html); len(m) > 1 {
		fmt.Sscanf(m[1], "%d", &metadata.Duration)
	}
	if m := patterns["poster"].FindStringSubmatch(html); len(m) > 1 {
		metadata.Poster = m[1]
	}

	videoMatches := patterns["videos"].FindAllStringSubmatch(html, -1)
	for _, m := range videoMatches {
		if len(m) > 2 {
			url := strings.ReplaceAll(m[1], "\\u0026", "&")
			metadata.Videos = append(metadata.Videos, struct {
				Name string
				URL  string
			}{m[2], url})
		}
	}

	if len(metadata.Videos) == 0 {
		return nil
	}

	return metadata
}

func (s *OKService) extractVideoID(urlStr string) string {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`/video/(\d+)`),
		regexp.MustCompile(`/live/(\d+)`),
	}
	for _, p := range patterns {
		matches := p.FindStringSubmatch(urlStr)
		if len(matches) > 1 {
			return matches[1]
		}
	}
	return ""
}

func (s *OKService) parseQuality(name string) Quality {
	name = strings.ToLower(name)
	switch {
	case strings.Contains(name, "1080"):
		return Quality1080p
	case strings.Contains(name, "720"):
		return Quality720p
	case strings.Contains(name, "480"):
		return Quality480p
	case strings.Contains(name, "360"):
		return Quality360p
	}
	return QualityBest
}

func (s *OKService) SupportsQuality() []Quality {
	return []Quality{QualityBest, Quality1080p, Quality720p, Quality480p}
}

func (s *OKService) SupportsMediaType() []MediaType {
	return []MediaType{MediaTypeVideo}
}