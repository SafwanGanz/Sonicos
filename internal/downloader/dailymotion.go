package downloader

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
)

type DailymotionService struct {
	*BaseService
}

func NewDailymotionService(client *HTTPClient) *DailymotionService {
	return &DailymotionService{
		BaseService: NewBaseService("dailymotion", []string{
			`https?://(?:www\.)?dailymotion\.com/video/([A-Za-z0-9]+)`,
			`https?://dai\.ly/([A-Za-z0-9]+)`,
		}, client),
	}
}

func (s *DailymotionService) Extract(ctx context.Context, urlStr string) ([]DownloadResult, error) {
	urlStr = s.cleanURL(urlStr)
	videoID := s.extractVideoID(urlStr)
	if videoID == "" {
		return nil, fmt.Errorf("could not extract video ID")
	}

	apiURL := fmt.Sprintf("https://www.dailymotion.com/player/metadata/video/%s", videoID)
	req := s.newRequest(ctx, "GET", apiURL)
	req.Header.Set("Referer", "https://www.dailymotion.com/")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var data struct {
		Title       string `json:"title"`
		Duration    int    `json:"duration"`
		Owner       struct {
			Username string `json:"username"`
		} `json:"owner"`
		Thumbnails []struct {
			URL string `json:"url"`
		} `json:"thumbnails"`
		Qualities map[string]struct {
			URLs []string `json:"urls"`
			Type string `json:"type"`
		} `json:"qualities"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}

	title := sanitizeFilename(data.Title)
	thumbnail := ""
	if len(data.Thumbnails) > 0 {
		thumbnail = data.Thumbnails[len(data.Thumbnails)-1].URL
	}

	var results []DownloadResult
	qualityOrder := []string{"1080", "720", "480", "380", "240", "144", "auto"}
	
	for _, q := range qualityOrder {
		if qual, ok := data.Qualities[q]; ok && len(qual.URLs) > 0 {
			result := DownloadResult{
				URL:        qual.URLs[0],
				Title:      title,
				MediaType:  MediaTypeVideo,
				Quality:    s.parseQualityLabel(q),
				Extension:  "mp4",
				Duration:   data.Duration,
				Thumbnail:  thumbnail,
				DirectURL:  true,
				Filename:   fmt.Sprintf("%s_%s.mp4", title, q),
				Metadata: map[string]string{
					"service":   "dailymotion",
					"video_id":  videoID,
					"owner":     data.Owner.Username,
					"quality":   q,
				},
			}
			results = append(results, result)
		}
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("no video sources found")
	}

	return results, nil
}

func (s *DailymotionService) extractVideoID(urlStr string) string {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`/video/([A-Za-z0-9]+)`),
		regexp.MustCompile(`dai\.ly/([A-Za-z0-9]+)`),
	}
	for _, p := range patterns {
		matches := p.FindStringSubmatch(urlStr)
		if len(matches) > 1 {
			return matches[1]
		}
	}
	return ""
}

func (s *DailymotionService) parseQualityLabel(label string) Quality {
	switch label {
	case "1080":
		return Quality1080p
	case "720":
		return Quality720p
	case "480":
		return Quality480p
	case "380":
		return Quality360p
	}
	return QualityBest
}

func (s *DailymotionService) SupportsQuality() []Quality {
	return []Quality{QualityBest, Quality1080p, Quality720p, Quality480p}
}

func (s *DailymotionService) SupportsMediaType() []MediaType {
	return []MediaType{MediaTypeVideo}
}