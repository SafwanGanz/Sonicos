package downloader

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
)

type VKService struct {
	*BaseService
}

func NewVKService(client *HTTPClient) *VKService {
	return &VKService{
		BaseService: NewBaseService("vk", []string{
			`https?://(?:www\.)?vk\.com/video(-?\d+)_(\d+)`,
			`https?://(?:www\.)?vk\.com/clip(-?\d+)_(\d+)`,
		}, client),
	}
}

func (s *VKService) Extract(ctx context.Context, urlStr string) ([]DownloadResult, error) {
	urlStr = s.cleanURL(urlStr)
	ownerID, videoID := s.extractIDs(urlStr)
	if ownerID == "" || videoID == "" {
		return nil, fmt.Errorf("could not extract video IDs")
	}

	apiURL := fmt.Sprintf("https://api.vk.com/method/video.get?videos=%s_%s&access_token=SERVICE_TOKEN&v=5.131", ownerID, videoID)
	req := s.newRequest(ctx, "GET", apiURL)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var data struct {
		Response struct {
			Items []struct {
				ID       int64  `json:"id"`
				OwnerID  int64  `json:"owner_id"`
				Title    string `json:"title"`
				Duration int    `json:"duration"`
				Date     int64  `json:"date"`
				Image    []struct {
					URL    string `json:"url"`
					Width  int    `json:"width"`
					Height int    `json:"height"`
				} `json:"image"`
				Files  map[string]string `json:"files"`
				Player string            `json:"player"`
			} `json:"items"`
		} `json:"response"`
		Error struct {
			ErrorCode int    `json:"error_code"`
			ErrorMsg  string `json:"error_msg"`
		} `json:"error"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}

	if data.Error.ErrorCode != 0 {
		return nil, fmt.Errorf("vk api error: %s", data.Error.ErrorMsg)
	}

	if len(data.Response.Items) == 0 {
		return nil, fmt.Errorf("video not found")
	}

	video := data.Response.Items[0]
	title := sanitizeFilename(video.Title)
	if title == "" {
		title = fmt.Sprintf("vk_%s_%s", ownerID, videoID)
	}

	thumbnail := ""
	if len(video.Image) > 0 {
		thumbnail = video.Image[len(video.Image)-1].URL
	}

	var results []DownloadResult

	qualityOrder := []string{"1080", "720", "480", "360", "240", "144"}
	for _, q := range qualityOrder {
		if url, ok := video.Files[fmt.Sprintf("mp4_%s", q)]; ok && url != "" {
			result := DownloadResult{
				URL:       url,
				Title:     title,
				MediaType: MediaTypeVideo,
				Quality:   s.parseQuality(q),
				Extension: "mp4",
				Duration:  video.Duration,
				Thumbnail: thumbnail,
				DirectURL: true,
				Filename:  fmt.Sprintf("%s_%s.mp4", title, q),
				Metadata: map[string]string{
					"service":  "vk",
					"video_id": fmt.Sprintf("%d", video.ID),
					"owner_id": fmt.Sprintf("%d", video.OwnerID),
					"quality":  q,
				},
			}
			results = append(results, result)
		}
	}

	if len(results) == 0 && video.Player != "" {
		result := DownloadResult{
			URL:       video.Player,
			Title:     title,
			MediaType: MediaTypeVideo,
			Quality:   QualityBest,
			Extension: "html",
			Duration:  video.Duration,
			Thumbnail: thumbnail,
			DirectURL: true,
			Filename:  fmt.Sprintf("%s.html", title),
			Metadata: map[string]string{
				"service":  "vk",
				"video_id": fmt.Sprintf("%d", video.ID),
				"type":     "player",
			},
		}
		results = append(results, result)
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("no video sources found")
	}

	return results, nil
}

func (s *VKService) extractIDs(urlStr string) (string, string) {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`/video(-?\d+)_(\d+)`),
		regexp.MustCompile(`/clip(-?\d+)_(\d+)`),
	}
	for _, p := range patterns {
		matches := p.FindStringSubmatch(urlStr)
		if len(matches) > 2 {
			return matches[1], matches[2]
		}
	}
	return "", ""
}

func (s *VKService) parseQuality(q string) Quality {
	switch q {
	case "1080":
		return Quality1080p
	case "720":
		return Quality720p
	case "480":
		return Quality480p
	case "360":
		return Quality360p
	}
	return QualityBest
}

func (s *VKService) SupportsQuality() []Quality {
	return []Quality{QualityBest, Quality1080p, Quality720p, Quality480p}
}

func (s *VKService) SupportsMediaType() []MediaType {
	return []MediaType{MediaTypeVideo}
}
