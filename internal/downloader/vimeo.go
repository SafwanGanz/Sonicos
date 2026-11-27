package downloader

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

type VimeoService struct {
	*BaseService
}

func NewVimeoService(client *HTTPClient) *VimeoService {
	return &VimeoService{
		BaseService: NewBaseService("vimeo", []string{
			`https?://(?:www\.)?vimeo\.com/(\d+)`,
			`https?://player\.vimeo\.com/video/(\d+)`,
		}, client),
	}
}

func (s *VimeoService) Extract(ctx context.Context, urlStr string) ([]DownloadResult, error) {
	urlStr = s.cleanURL(urlStr)
	videoID := s.extractVideoID(urlStr)
	if videoID == "" {
		return nil, fmt.Errorf("could not extract video ID")
	}

	apiURL := fmt.Sprintf("https://player.vimeo.com/video/%s/config", videoID)
	req := s.newRequest(ctx, "GET", apiURL)
	req.Header.Set("Referer", "https://vimeo.com/")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var data struct {
		Video struct {
			Title       string `json:"title"`
			Duration    int    `json:"duration"`
			Thumbnails  struct {
				Base  string `json:"base"`
				Sizes []struct {
					Width  int    `json:"width"`
					Height int    `json:"height"`
					URL    string `json:"url"`
				} `json:"sizes"`
			} `json:"thumbnails"`
		} `json:"video"`
		Request struct {
			Files struct {
				Progressive []struct {
					URL      string `json:"url"`
					Mime     string `json:"mime"`
					Width    int    `json:"width"`
					Height   int    `json:"height"`
					Quality  string `json:"quality"`
				} `json:"progressive"`
				HLS struct {
					Default string `json:"default"`
				} `json:"hls"`
				Dash struct {
					Default string `json:"default"`
				} `json:"dash"`
			} `json:"files"`
		} `json:"request"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}

	if data.Request.Files.Progressive == nil && data.Request.Files.HLS.Default == "" && data.Request.Files.Dash.Default == "" {
		return nil, fmt.Errorf("no video sources found")
	}

	title := sanitizeFilename(data.Video.Title)
	thumbnail := ""
	if len(data.Video.Thumbnails.Sizes) > 0 {
		thumbnail = data.Video.Thumbnails.Sizes[len(data.Video.Thumbnails.Sizes)-1].URL
	}

	var results []DownloadResult

	for _, prog := range data.Request.Files.Progressive {
		if prog.URL == "" {
			continue
		}
		quality := s.parseQualityFromLabel(prog.Quality, prog.Height)
		result := DownloadResult{
			URL:        prog.URL,
			Title:      title,
			MediaType:  MediaTypeVideo,
			Quality:    quality,
			Extension:  "mp4",
			Duration:   data.Video.Duration,
			Thumbnail:  thumbnail,
			DirectURL:  true,
			Filename:   fmt.Sprintf("%s_%s.mp4", title, prog.Quality),
			Metadata: map[string]string{
				"service":   "vimeo",
				"video_id":  videoID,
				"mime_type": prog.Mime,
			},
		}
		if prog.Width > 0 && prog.Height > 0 {
			result.Metadata["width"] = fmt.Sprintf("%d", prog.Width)
			result.Metadata["height"] = fmt.Sprintf("%d", prog.Height)
		}
		results = append(results, result)
	}

	if data.Request.Files.HLS.Default != "" {
		result := DownloadResult{
			URL:        data.Request.Files.HLS.Default,
			Title:      title,
			MediaType:  MediaTypeVideo,
			Quality:    QualityBest,
			Extension:  "m3u8",
			Duration:   data.Video.Duration,
			Thumbnail:  thumbnail,
			DirectURL:  true,
			Filename:   fmt.Sprintf("%s.m3u8", title),
			Metadata: map[string]string{
				"service":  "vimeo",
				"video_id": videoID,
				"type":     "hls",
			},
		}
		results = append(results, result)
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("no downloadable formats found")
	}

	return results, nil
}

func (s *VimeoService) extractVideoID(urlStr string) string {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`/(\d+)(?:\?|$)`),
	}
	for _, p := range patterns {
		matches := p.FindStringSubmatch(urlStr)
		if len(matches) > 1 {
			return matches[1]
		}
	}
	return ""
}

func (s *VimeoService) parseQualityFromLabel(label string, height int) Quality {
	if height >= 1080 {
		return Quality1080p
	}
	if height >= 720 {
		return Quality720p
	}
	if height >= 480 {
		return Quality480p
	}
	if height >= 360 {
		return Quality360p
	}
	switch strings.ToLower(label) {
	case "1080p":
		return Quality1080p
	case "720p":
		return Quality720p
	case "480p", "sd":
		return Quality480p
	case "360p":
		return Quality360p
	}
	return QualityBest
}

func (s *VimeoService) SupportsQuality() []Quality {
	return []Quality{QualityBest, Quality1080p, Quality720p, Quality480p}
}

func (s *VimeoService) SupportsMediaType() []MediaType {
	return []MediaType{MediaTypeVideo}
}