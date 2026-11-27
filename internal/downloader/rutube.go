package downloader

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
)

type RutubeService struct {
	*BaseService
}

func NewRutubeService(client *HTTPClient) *RutubeService {
	return &RutubeService{
		BaseService: NewBaseService("rutube", []string{
			`https?://(?:www\.)?rutube\.ru/video/([A-Za-z0-9]+)`,
			`https?://(?:www\.)?rutube\.ru/play/embed/([A-Za-z0-9]+)`,
		}, client),
	}
}

func (s *RutubeService) Extract(ctx context.Context, urlStr string) ([]DownloadResult, error) {
	urlStr = s.cleanURL(urlStr)
	videoID := s.extractVideoID(urlStr)
	if videoID == "" {
		return nil, fmt.Errorf("could not extract video ID")
	}

	apiURL := fmt.Sprintf("https://rutube.ru/api/video/%s/", videoID)
	req := s.newRequest(ctx, "GET", apiURL)
	req.Header.Set("Referer", "https://rutube.ru/")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var data struct {
		Title       string `json:"title"`
		Duration    int    `json:"duration"`
		Author      struct {
			Name string `json:"name"`
		} `json:"author"`
		Thumbnail   string `json:"thumbnail_url"`
		VideoURL    string `json:"video_url"`
		VideoM3U8   string `json:"m3u8_url"`
		VideoBalancer string `json:"video_balancer"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}

	title := sanitizeFilename(data.Title)
	if title == "" {
		title = fmt.Sprintf("rutube_%s", videoID)
	}

	var results []DownloadResult

	if data.VideoM3U8 != "" {
		result := DownloadResult{
			URL:        data.VideoM3U8,
			Title:      title,
			MediaType:  MediaTypeVideo,
			Quality:    QualityBest,
			Extension:  "m3u8",
			Duration:   data.Duration,
			Thumbnail:  data.Thumbnail,
			DirectURL:  true,
			Filename:   fmt.Sprintf("%s.m3u8", title),
			Metadata: map[string]string{
				"service":   "rutube",
				"video_id":  videoID,
				"author":    data.Author.Name,
				"type":      "hls",
			},
		}
		results = append(results, result)
	}

	if data.VideoURL != "" {
		result := DownloadResult{
			URL:        data.VideoURL,
			Title:      title,
			MediaType:  MediaTypeVideo,
			Quality:    QualityBest,
			Extension:  "mp4",
			Duration:   data.Duration,
			Thumbnail:  data.Thumbnail,
			DirectURL:  true,
			Filename:   fmt.Sprintf("%s.mp4", title),
			Metadata: map[string]string{
				"service":   "rutube",
				"video_id":  videoID,
				"author":    data.Author.Name,
				"type":      "mp4",
			},
		}
		results = append(results, result)
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("no video sources found")
	}

	return results, nil
}

func (s *RutubeService) extractVideoID(urlStr string) string {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`/video/([A-Za-z0-9]+)`),
		regexp.MustCompile(`/play/embed/([A-Za-z0-9]+)`),
	}
	for _, p := range patterns {
		matches := p.FindStringSubmatch(urlStr)
		if len(matches) > 1 {
			return matches[1]
		}
	}
	return ""
}

func (s *RutubeService) SupportsQuality() []Quality {
	return []Quality{QualityBest}
}

func (s *RutubeService) SupportsMediaType() []MediaType {
	return []MediaType{MediaTypeVideo}
}