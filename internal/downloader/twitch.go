package downloader

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

type TwitchService struct {
	*BaseService
}

func NewTwitchService(client *HTTPClient) *TwitchService {
	return &TwitchService{
		BaseService: NewBaseService("twitch", []string{
			`https?://(?:www\.)?twitch\.tv/videos/(\d+)`,
			`https?://(?:www\.)?twitch\.tv/[A-Za-z0-9_]+/clip/([A-Za-z0-9_-]+)`,
			`https?://clips\.twitch\.tv/([A-Za-z0-9_-]+)`,
		}, client),
	}
}

func (s *TwitchService) Extract(ctx context.Context, urlStr string) ([]DownloadResult, error) {
	urlStr = s.cleanURL(urlStr)
	
	if strings.Contains(urlStr, "/clip/") || strings.Contains(urlStr, "clips.twitch.tv") {
		return s.extractClip(ctx, urlStr)
	}
	return s.extractVOD(ctx, urlStr)
}

func (s *TwitchService) extractClip(ctx context.Context, urlStr string) ([]DownloadResult, error) {
	clipSlug := s.extractClipSlug(urlStr)
	if clipSlug == "" {
		return nil, fmt.Errorf("could not extract clip slug")
	}

	apiURL := fmt.Sprintf("https://gql.twitch.tv/gql")
	payload := []map[string]interface{}{
		{
			"operationName": "VideoAccessToken_Clip",
			"variables":     map[string]string{"slug": clipSlug},
			"extensions": map[string]interface{}{
				"persistedQuery": map[string]string{
					"version":    "1",
					"sha256Hash": "36b89d2507fce29e5ca551df756d27c1cfe079e2609642b4390aa4c35796eb11",
				},
			},
		},
	}

	json.Marshal(payload)
	req := s.newRequest(ctx, "POST", apiURL)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Client-ID", "kimne78kx3ncx6brgo4mv6wki5h1ko")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var data []struct {
		Data struct {
			Clip struct {
				Title     string `json:"title"`
				Thumbnail string `json:"thumbnailURL"`
				Duration  int    `json:"durationSeconds"`
				Broadcaster struct {
					DisplayName string `json:"displayName"`
				} `json:"broadcaster"`
				VideoQualities []struct {
					Quality string `json:"quality"`
					URL     string `json:"sourceURL"`
				} `json:"videoQualities"`
			} `json:"clip"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}

	if len(data) == 0 || data[0].Data.Clip.VideoQualities == nil {
		return nil, fmt.Errorf("clip data not found")
	}

	clip := data[0].Data.Clip
	title := sanitizeFilename(fmt.Sprintf("%s - %s", clip.Broadcaster.DisplayName, clip.Title))

	var results []DownloadResult
	for _, vq := range clip.VideoQualities {
		if vq.URL == "" {
			continue
		}
		result := DownloadResult{
			URL:        vq.URL,
			Title:      title,
			MediaType:  MediaTypeVideo,
			Quality:    s.parseQuality(vq.Quality),
			Extension:  "mp4",
			Duration:   clip.Duration,
			Thumbnail:  clip.Thumbnail,
			DirectURL:  true,
			Filename:   fmt.Sprintf("%s_%s.mp4", title, vq.Quality),
			Metadata: map[string]string{
				"service":      "twitch",
				"clip_slug":    clipSlug,
				"broadcaster":  clip.Broadcaster.DisplayName,
				"quality":      vq.Quality,
			},
		}
		results = append(results, result)
	}

	return results, nil
}

func (s *TwitchService) extractVOD(ctx context.Context, urlStr string) ([]DownloadResult, error) {
	vodID := s.extractVODID(urlStr)
	if vodID == "" {
		return nil, fmt.Errorf("could not extract VOD ID")
	}

	apiURL := fmt.Sprintf("https://gql.twitch.tv/gql")
	payload := []map[string]interface{}{
		{
			"operationName": "VideoMetadata",
			"variables":     map[string]string{"videoID": vodID},
			"extensions": map[string]interface{}{
				"persistedQuery": map[string]string{
					"version":    "1",
					"sha256Hash": "a01b8b5e5f2a7c3d8e9f1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e",
				},
			},
		},
	}

	json.Marshal(payload)
	req := s.newRequest(ctx, "POST", apiURL)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Client-ID", "kimne78kx3ncx6brgo4mv6wki5h1ko")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var data []struct {
		Data struct {
			Video struct {
				Title     string `json:"title"`
				Length    int    `json:"lengthSeconds"`
				Preview   string `json:"previewThumbnailURL"`
				Owner     struct {
					DisplayName string `json:"displayName"`
				} `json:"owner"`
				PlaybackAccessToken struct {
					Signature string `json:"signature"`
					Value     string `json:"value"`
				} `json:"playbackAccessToken"`
			} `json:"video"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}

	if len(data) == 0 {
		return nil, fmt.Errorf("VOD data not found")
	}

	vod := data[0].Data.Video
	title := sanitizeFilename(fmt.Sprintf("%s - %s", vod.Owner.DisplayName, vod.Title))

	token := vod.PlaybackAccessToken.Value
	signature := vod.PlaybackAccessToken.Signature

	playlistURL := fmt.Sprintf("https://usher.ttvnw.net/vod/%s.m3u8?client_id=kimne78kx3ncx6brgo4mv6wki5h1ko&player=twitchweb&token=%s&sig=%s", vodID, url.QueryEscape(token), signature)
	
	result := DownloadResult{
		URL:        playlistURL,
		Title:      title,
		MediaType:  MediaTypeVideo,
		Quality:    QualityBest,
		Extension:  "m3u8",
		Duration:   vod.Length,
		Thumbnail:  vod.Preview,
		DirectURL:  true,
		Filename:   fmt.Sprintf("%s.m3u8", title),
		Metadata: map[string]string{
			"service":  "twitch",
			"vod_id":   vodID,
			"owner":    vod.Owner.DisplayName,
			"type":     "vod",
		},
	}

	return []DownloadResult{result}, nil
}

func (s *TwitchService) extractClipSlug(urlStr string) string {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`/clip/([A-Za-z0-9_-]+)`),
		regexp.MustCompile(`clips\.twitch\.tv/([A-Za-z0-9_-]+)`),
	}
	for _, p := range patterns {
		matches := p.FindStringSubmatch(urlStr)
		if len(matches) > 1 {
			return matches[1]
		}
	}
	return ""
}

func (s *TwitchService) extractVODID(urlStr string) string {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`/videos/(\d+)`),
	}
	for _, p := range patterns {
		matches := p.FindStringSubmatch(urlStr)
		if len(matches) > 1 {
			return matches[1]
		}
	}
	return ""
}

func (s *TwitchService) parseQuality(label string) Quality {
	switch strings.ToLower(label) {
	case "1080p60", "1080p":
		return Quality1080p
	case "720p60", "720p":
		return Quality720p
	case "480p":
		return Quality480p
	case "360p":
		return Quality360p
	}
	return QualityBest
}

func (s *TwitchService) SupportsQuality() []Quality {
	return []Quality{QualityBest, Quality1080p, Quality720p, Quality480p}
}

func (s *TwitchService) SupportsMediaType() []MediaType {
	return []MediaType{MediaTypeVideo}
}