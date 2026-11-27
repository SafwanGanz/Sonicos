package downloader

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
)

type SnapchatService struct {
	*BaseService
}

func NewSnapchatService(client *HTTPClient) *SnapchatService {
	return &SnapchatService{
		BaseService: NewBaseService("snapchat", []string{
			`https?://(?:www\.)?snapchat\.com/spotlight/([A-Za-z0-9]+)`,
			`https?://(?:www\.)?snapchat\.com/discover/([A-Za-z0-9]+)`,
			`https?://(?:www\.)?snapchat\.com/@[A-Za-z0-9_.]+/([A-Za-z0-9]+)`,
		}, client),
	}
}

func (s *SnapchatService) Extract(ctx context.Context, urlStr string) ([]DownloadResult, error) {
	urlStr = s.cleanURL(urlStr)
	itemID := s.extractItemID(urlStr)
	if itemID == "" {
		return nil, fmt.Errorf("could not extract item ID")
	}

	apiURL := fmt.Sprintf("https://api.snapchat.com/v1/spotlight/%s", itemID)
	req := s.newRequest(ctx, "GET", apiURL)
	req.Header.Set("Referer", "https://www.snapchat.com/")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var data struct {
		Story struct {
			ID        string `json:"id"`
			Title     string `json:"title"`
			Author    struct {
				Username string `json:"username"`
			} `json:"author"`
			SnapList []struct {
				ID          string `json:"id"`
				MediaURL    string `json:"media_url"`
				MediaType   string `json:"media_type"`
				Duration    int    `json:"duration"`
				Thumbnail   string `json:"thumbnail_url"`
			} `json:"snap_list"`
		} `json:"story"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}

	if len(data.Story.SnapList) == 0 {
		return nil, fmt.Errorf("no media found")
	}

	title := sanitizeFilename(data.Story.Title)
	if title == "" {
		title = fmt.Sprintf("snapchat_%s", itemID)
	}

	var results []DownloadResult
	for _, snap := range data.Story.SnapList {
		if snap.MediaURL == "" {
			continue
		}
		mediaType := MediaTypeVideo
		if snap.MediaType == "image" {
			mediaType = MediaTypeImage
		}
		ext := "mp4"
		if mediaType == MediaTypeImage {
			ext = "jpg"
		}
		result := DownloadResult{
			URL:        snap.MediaURL,
			Title:      title,
			MediaType:  mediaType,
			Quality:    QualityBest,
			Extension:  ext,
			Duration:   snap.Duration,
			Thumbnail:  snap.Thumbnail,
			DirectURL:  true,
			Filename:   fmt.Sprintf("%s_%s.%s", title, snap.ID, ext),
			Metadata: map[string]string{
				"service":    "snapchat",
				"story_id":   itemID,
				"snap_id":    snap.ID,
				"author":     data.Story.Author.Username,
			},
		}
		results = append(results, result)
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("no downloadable media found")
	}

	return results, nil
}

func (s *SnapchatService) extractItemID(urlStr string) string {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`/spotlight/([A-Za-z0-9]+)`),
		regexp.MustCompile(`/discover/([A-Za-z0-9]+)`),
		regexp.MustCompile(`@[A-Za-z0-9_.]+/([A-Za-z0-9]+)`),
	}
	for _, p := range patterns {
		matches := p.FindStringSubmatch(urlStr)
		if len(matches) > 1 {
			return matches[1]
		}
	}
	return ""
}

func (s *SnapchatService) SupportsQuality() []Quality {
	return []Quality{QualityBest}
}

func (s *SnapchatService) SupportsMediaType() []MediaType {
	return []MediaType{MediaTypeVideo, MediaTypeImage}
}