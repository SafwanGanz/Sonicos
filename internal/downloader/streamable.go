package downloader

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
)

type StreamableService struct {
	*BaseService
}

func NewStreamableService(client *HTTPClient) *StreamableService {
	return &StreamableService{
		BaseService: NewBaseService("streamable", []string{
			`https?://(?:www\.)?streamable\.com/([A-Za-z0-9]+)`,
		}, client),
	}
}

func (s *StreamableService) Extract(ctx context.Context, urlStr string) ([]DownloadResult, error) {
	urlStr = s.cleanURL(urlStr)
	videoID := s.extractVideoID(urlStr)
	if videoID == "" {
		return nil, fmt.Errorf("could not extract video ID")
	}

	apiURL := fmt.Sprintf("https://api.streamable.com/videos/%s", videoID)
	req := s.newRequest(ctx, "GET", apiURL)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var data struct {
		Title       string `json:"title"`
		Duration    float64 `json:"duration"`
		Filesize    int64  `json:"filesize"`
		Thumbnail   string `json:"thumbnail_url"`
		Owner       struct {
			Username string `json:"username"`
		} `json:"owner"`
		Files map[string]struct {
			URL       string `json:"url"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
			Bitrate   int    `json:"bitrate"`
			Extension string `json:"extension"`
		} `json:"files"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}

	title := sanitizeFilename(data.Title)
	if title == "" {
		title = fmt.Sprintf("streamable_%s", videoID)
	}

	var results []DownloadResult
	for name, file := range data.Files {
		if file.URL == "" {
			continue
		}
		quality := s.parseQuality(file.Height)
		ext := file.Extension
		if ext == "" {
			ext = "mp4"
		}
		result := DownloadResult{
			URL:        file.URL,
			Title:      title,
			MediaType:  MediaTypeVideo,
			Quality:    quality,
			Extension:  ext,
			Duration:   int(data.Duration),
			Thumbnail:  data.Thumbnail,
			Filesize:   int64(float64(file.Bitrate) * data.Duration / 8),
			DirectURL:  true,
			Filename:   fmt.Sprintf("%s_%s.%s", title, name, ext),
			Metadata: map[string]string{
				"service":   "streamable",
				"video_id":  videoID,
				"owner":     data.Owner.Username,
				"variant":   name,
			},
		}
		if file.Width > 0 && file.Height > 0 {
			result.Metadata["width"] = fmt.Sprintf("%d", file.Width)
			result.Metadata["height"] = fmt.Sprintf("%d", file.Height)
		}
		results = append(results, result)
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("no video sources found")
	}

	return results, nil
}

func (s *StreamableService) extractVideoID(urlStr string) string {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`/([A-Za-z0-9]+)(?:\?|$)`),
	}
	for _, p := range patterns {
		matches := p.FindStringSubmatch(urlStr)
		if len(matches) > 1 {
			return matches[1]
		}
	}
	return ""
}

func (s *StreamableService) parseQuality(height int) Quality {
	switch {
	case height >= 1080:
		return Quality1080p
	case height >= 720:
		return Quality720p
	case height >= 480:
		return Quality480p
	default:
		return Quality360p
	}
}

func (s *StreamableService) SupportsQuality() []Quality {
	return []Quality{QualityBest, Quality1080p, Quality720p, Quality480p}
}

func (s *StreamableService) SupportsMediaType() []MediaType {
	return []MediaType{MediaTypeVideo}
}