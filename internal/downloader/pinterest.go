package downloader

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
)

type PinterestService struct {
	*BaseService
}

func NewPinterestService(client *HTTPClient) *PinterestService {
	return &PinterestService{
		BaseService: NewBaseService("pinterest", []string{
			`https?://(?:www\.)?pinterest\.com/pin/(\d+)`,
			`https?://(?:www\.)?pinterest\.com/[A-Za-z0-9_]+/pin/(\d+)`,
			`https?://pin\.it/([A-Za-z0-9]+)`,
		}, client),
	}
}

func (s *PinterestService) Extract(ctx context.Context, urlStr string) ([]DownloadResult, error) {
	urlStr = s.cleanURL(urlStr)
	pinID := s.extractPinID(urlStr)
	if pinID == "" {
		return nil, fmt.Errorf("could not extract pin ID")
	}

	apiURL := fmt.Sprintf("https://www.pinterest.com/resource/PinResource/get/?source_url=/pin/%s/&data=%%7B%%22options%%22%%3A%%7B%%22pin%%22%%3A%%22%s%%22%%7D%%7D", pinID, pinID)
	req := s.newRequest(ctx, "GET", apiURL)
	req.Header.Set("Referer", "https://www.pinterest.com/")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var data struct {
		ResourceResponse struct {
			Data struct {
				ID          string `json:"id"`
				Title       string `json:"title"`
				Description string `json:"description"`
				Images      struct {
					Orig struct {
						URL    string `json:"url"`
						Width  int    `json:"width"`
						Height int    `json:"height"`
					} `json:"orig"`
				} `json:"images"`
				Videos struct {
					VideoList []struct {
						URL    string `json:"url"`
						Width  int    `json:"width"`
						Height int    `json:"height"`
					} `json:"video_list"`
				} `json:"videos"`
				Media struct {
					Videos []struct {
						URL    string `json:"url"`
						Width  int    `json:"width"`
						Height int    `json:"height"`
					} `json:"videos"`
				} `json:"media"`
				Pinner struct {
					Username string `json:"username"`
				} `json:"pinner"`
			} `json:"data"`
		} `json:"resource_response"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}

	pin := data.ResourceResponse.Data
	title := sanitizeFilename(pin.Title)
	if title == "" {
		title = fmt.Sprintf("pinterest_%s", pin.ID)
	}

	var results []DownloadResult

	for _, v := range pin.Videos.VideoList {
		if v.URL != "" {
			result := DownloadResult{
				URL:       v.URL,
				Title:     title,
				MediaType: MediaTypeVideo,
				Quality:   s.parseQuality(v.Height),
				Extension: "mp4",
				DirectURL: true,
				Filename:  fmt.Sprintf("%s.mp4", title),
				Metadata: map[string]string{
					"service": "pinterest",
					"pin_id":  pin.ID,
					"author":  pin.Pinner.Username,
				},
			}
			if v.Width > 0 && v.Height > 0 {
				result.Metadata["width"] = fmt.Sprintf("%d", v.Width)
				result.Metadata["height"] = fmt.Sprintf("%d", v.Height)
			}
			results = append(results, result)
		}
	}

	for _, v := range pin.Media.Videos {
		if v.URL != "" {
			result := DownloadResult{
				URL:       v.URL,
				Title:     title,
				MediaType: MediaTypeVideo,
				Quality:   s.parseQuality(v.Height),
				Extension: "mp4",
				DirectURL: true,
				Filename:  fmt.Sprintf("%s.mp4", title),
				Metadata: map[string]string{
					"service": "pinterest",
					"pin_id":  pin.ID,
					"author":  pin.Pinner.Username,
				},
			}
			if v.Width > 0 && v.Height > 0 {
				result.Metadata["width"] = fmt.Sprintf("%d", v.Width)
				result.Metadata["height"] = fmt.Sprintf("%d", v.Height)
			}
			results = append(results, result)
		}
	}

	if pin.Images.Orig.URL != "" {
		result := DownloadResult{
			URL:       pin.Images.Orig.URL,
			Title:     title,
			MediaType: MediaTypeImage,
			Quality:   QualityBest,
			Extension: "jpg",
			DirectURL: true,
			Filename:  fmt.Sprintf("%s.jpg", title),
			Metadata: map[string]string{
				"service": "pinterest",
				"pin_id":  pin.ID,
				"author":  pin.Pinner.Username,
			},
		}
		results = append(results, result)
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("no media found in pin")
	}

	return results, nil
}

func (s *PinterestService) extractPinID(urlStr string) string {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`/pin/(\d+)`),
		regexp.MustCompile(`pin\.it/([A-Za-z0-9]+)`),
	}
	for _, p := range patterns {
		matches := p.FindStringSubmatch(urlStr)
		if len(matches) > 1 {
			return matches[1]
		}
	}
	return ""
}

func (s *PinterestService) parseQuality(height int) Quality {
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

func (s *PinterestService) SupportsQuality() []Quality {
	return []Quality{QualityBest, Quality1080p, Quality720p, Quality480p}
}

func (s *PinterestService) SupportsMediaType() []MediaType {
	return []MediaType{MediaTypeVideo, MediaTypeImage}
}
