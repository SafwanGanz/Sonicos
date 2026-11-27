package downloader

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
)

type FacebookService struct {
	*BaseService
}

func NewFacebookService(client *HTTPClient) *FacebookService {
	return &FacebookService{
		BaseService: NewBaseService("facebook", []string{
			`https?://(?:www\.)?facebook\.com/[A-Za-z0-9_.]+/videos/(\d+)`,
			`https?://(?:www\.)?fb\.watch/([A-Za-z0-9_-]+)`,
			`https?://(?:www\.)?facebook\.com/watch/?\?v=(\d+)`,
			`https?://(?:www\.)?facebook\.com/reel/(\d+)`,
		}, client),
	}
}

func (s *FacebookService) Extract(ctx context.Context, urlStr string) ([]DownloadResult, error) {
	urlStr = s.cleanURL(urlStr)
	videoID := s.extractVideoID(urlStr)
	if videoID == "" {
		return nil, fmt.Errorf("could not extract video ID")
	}

	apiURL := fmt.Sprintf("https://www.facebook.com/video/video_data/?video_id=%s", videoID)
	req := s.newRequest(ctx, "GET", apiURL)
	req.Header.Set("Referer", "https://www.facebook.com/")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	var results []DownloadResult

	if strings.HasPrefix(bodyStr, "for (;;);") {
		bodyStr = strings.TrimPrefix(bodyStr, "for (;;);")
	}

	var data struct {
		Video struct {
			Title           string `json:"title"`
			Description     string `json:"description"`
			Duration        int    `json:"duration"`
			Preview         string `json:"preview"`
			BrowserNativeSD struct {
				URL string `json:"url"`
			} `json:"browser_native_sd"`
			BrowserNativeHD struct {
				URL string `json:"url"`
			} `json:"browser_native_hd"`
		} `json:"video"`
	}

	if err := json.Unmarshal([]byte(bodyStr), &data); err != nil {
		return nil, err
	}

	title := sanitizeFilename(data.Video.Title)
	if title == "" {
		title = fmt.Sprintf("facebook_%s", videoID)
	}

	if data.Video.BrowserNativeHD.URL != "" {
		result := DownloadResult{
			URL:       data.Video.BrowserNativeHD.URL,
			Title:     title,
			MediaType: MediaTypeVideo,
			Quality:   Quality720p,
			Extension: "mp4",
			Duration:  data.Video.Duration,
			Thumbnail: data.Video.Preview,
			DirectURL: true,
			Filename:  fmt.Sprintf("%s_hd.mp4", title),
			Metadata: map[string]string{
				"service":  "facebook",
				"video_id": videoID,
				"quality":  "hd",
			},
		}
		results = append(results, result)
	}

	if data.Video.BrowserNativeSD.URL != "" {
		result := DownloadResult{
			URL:       data.Video.BrowserNativeSD.URL,
			Title:     title,
			MediaType: MediaTypeVideo,
			Quality:   Quality480p,
			Extension: "mp4",
			Duration:  data.Video.Duration,
			Thumbnail: data.Video.Preview,
			DirectURL: true,
			Filename:  fmt.Sprintf("%s_sd.mp4", title),
			Metadata: map[string]string{
				"service":  "facebook",
				"video_id": videoID,
				"quality":  "sd",
			},
		}
		results = append(results, result)
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("no video sources found")
	}

	return results, nil
}

func (s *FacebookService) extractVideoID(urlStr string) string {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`/videos/(\d+)`),
		regexp.MustCompile(`fb\.watch/([A-Za-z0-9_-]+)`),
		regexp.MustCompile(`[?&]v=(\d+)`),
		regexp.MustCompile(`/reel/(\d+)`),
	}
	for _, p := range patterns {
		matches := p.FindStringSubmatch(urlStr)
		if len(matches) > 1 {
			return matches[1]
		}
	}
	return ""
}

func (s *FacebookService) SupportsQuality() []Quality {
	return []Quality{QualityBest, Quality720p, Quality480p}
}

func (s *FacebookService) SupportsMediaType() []MediaType {
	return []MediaType{MediaTypeVideo}
}
