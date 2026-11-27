package downloader

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

type NewgroundsService struct {
	*BaseService
}

func NewNewgroundsService(client *HTTPClient) *NewgroundsService {
	return &NewgroundsService{
		BaseService: NewBaseService("newgrounds", []string{
			`https?://(?:www\.)?newgrounds\.com/portal/view/(\d+)`,
			`https?://(?:www\.)?newgrounds\.com/audio/listen/(\d+)`,
		}, client),
	}
}

func (s *NewgroundsService) Extract(ctx context.Context, urlStr string) ([]DownloadResult, error) {
	urlStr = s.cleanURL(urlStr)
	itemID := s.extractItemID(urlStr)
	if itemID == "" {
		return nil, fmt.Errorf("could not extract item ID")
	}

	apiURL := fmt.Sprintf("https://www.newgrounds.com/api/gateway.php?command=core.getPortals&portalIds=%s", itemID)
	req := s.newRequest(ctx, "GET", apiURL)
	req.Header.Set("Referer", "https://www.newgrounds.com/")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var data struct {
		Portals []struct {
			ID        string `json:"id"`
			Title     string `json:"title"`
			Author    string `json:"author"`
			Type      string `json:"type"`
			Video     struct {
				Files []struct {
					URL      string `json:"url"`
					Quality  string `json:"quality"`
					Format   string `json:"format"`
				} `json:"files"`
			} `json:"video"`
			Audio struct {
				Files []struct {
					URL     string `json:"url"`
					Quality string `json:"quality"`
				} `json:"files"`
			} `json:"audio"`
			Thumbnail string `json:"thumbnail"`
		} `json:"portals"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}

	if len(data.Portals) == 0 {
		return nil, fmt.Errorf("item not found")
	}

	item := data.Portals[0]
	title := sanitizeFilename(item.Title)

	var results []DownloadResult

	for _, vf := range item.Video.Files {
		if vf.URL != "" {
			result := DownloadResult{
				URL:        vf.URL,
				Title:      title,
				MediaType:  MediaTypeVideo,
				Quality:    s.parseQuality(vf.Quality),
				Extension:  vf.Format,
				DirectURL:  true,
				Filename:   fmt.Sprintf("%s_%s.%s", title, vf.Quality, vf.Format),
				Metadata: map[string]string{
					"service":   "newgrounds",
					"item_id":   itemID,
					"author":    item.Author,
					"quality":   vf.Quality,
				},
			}
			results = append(results, result)
		}
	}

	for _, af := range item.Audio.Files {
		if af.URL != "" {
			result := DownloadResult{
				URL:        af.URL,
				Title:      title,
				MediaType:  MediaTypeAudio,
				Quality:    QualityAudio,
				Extension:  "mp3",
				DirectURL:  true,
				Filename:   fmt.Sprintf("%s.mp3", title),
				Metadata: map[string]string{
					"service":  "newgrounds",
					"item_id":  itemID,
					"author":   item.Author,
					"quality":  af.Quality,
				},
			}
			results = append(results, result)
		}
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("no media found")
	}

	return results, nil
}

func (s *NewgroundsService) extractItemID(urlStr string) string {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`/portal/view/(\d+)`),
		regexp.MustCompile(`/audio/listen/(\d+)`),
	}
	for _, p := range patterns {
		matches := p.FindStringSubmatch(urlStr)
		if len(matches) > 1 {
			return matches[1]
		}
	}
	return ""
}

func (s *NewgroundsService) parseQuality(label string) Quality {
	switch strings.ToLower(label) {
	case "1080p":
		return Quality1080p
	case "720p":
		return Quality720p
	case "480p":
		return Quality480p
	}
	return QualityBest
}

func (s *NewgroundsService) SupportsQuality() []Quality {
	return []Quality{QualityBest, Quality1080p, Quality720p, Quality480p, QualityAudio}
}

func (s *NewgroundsService) SupportsMediaType() []MediaType {
	return []MediaType{MediaTypeVideo, MediaTypeAudio}
}