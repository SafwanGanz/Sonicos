package downloader

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

type SoundCloudService struct {
	*BaseService
}

func NewSoundCloudService(client *HTTPClient) *SoundCloudService {
	return &SoundCloudService{
		BaseService: NewBaseService("soundcloud", []string{
			`https?://(?:www\.)?soundcloud\.com/[A-Za-z0-9_-]+/[A-Za-z0-9_-]+`,
			`https?://soundcloud\.com/[A-Za-z0-9_-]+/[A-Za-z0-9_-]+`,
			`https?://(?:www\.)?soundcloud\.com/[A-Za-z0-9_-]+/sets/[A-Za-z0-9_-]+`,
		}, client),
	}
}

func (s *SoundCloudService) Extract(ctx context.Context, urlStr string) ([]DownloadResult, error) {
	urlStr = s.cleanURL(urlStr)
	resolveURL := fmt.Sprintf("https://api-v2.soundcloud.com/resolve?url=%s&client_id=YOUR_CLIENT_ID", url.QueryEscape(urlStr))

	req := s.newRequest(ctx, "GET", resolveURL)
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var data struct {
		Kind       string `json:"kind"`
		ID         int64  `json:"id"`
		Title      string `json:"title"`
		Duration   int    `json:"duration"`
		ArtworkURL string `json:"artwork_url"`
		User       struct {
			Username string `json:"username"`
		} `json:"user"`
		Media struct {
			Transcodings []struct {
				URL    string `json:"url"`
				Format struct {
					Protocol string `json:"protocol"`
					MimeType string `json:"mime_type"`
				} `json:"format"`
				Quality string `json:"quality"`
			} `json:"transcodings"`
		} `json:"media"`
		Tracks []struct {
			ID         int64  `json:"id"`
			Title      string `json:"title"`
			Duration   int    `json:"duration"`
			ArtworkURL string `json:"artwork_url"`
			User       struct {
				Username string `json:"username"`
			} `json:"user"`
			Media struct {
				Transcodings []struct {
					URL    string `json:"url"`
					Format struct {
						Protocol string `json:"protocol"`
						MimeType string `json:"mime_type"`
					} `json:"format"`
					Quality string `json:"quality"`
				} `json:"transcodings"`
			} `json:"media"`
		} `json:"tracks"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}

	var results []DownloadResult

	if data.Kind == "track" {
		results = append(results, s.parseTrack(data.Title, data.User.Username, data.Duration, data.ArtworkURL, data.Media.Transcodings, data.ID)...)
	} else if data.Kind == "playlist" || data.Kind == "system-playlist" {
		for _, track := range data.Tracks {
			results = append(results, s.parseTrack(track.Title, track.User.Username, track.Duration, track.ArtworkURL, track.Media.Transcodings, track.ID)...)
		}
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("no playable media found")
	}

	return results, nil
}

func (s *SoundCloudService) parseTrack(title, username string, duration int, artworkURL string, transcodings []struct {
	URL    string `json:"url"`
	Format struct {
		Protocol string `json:"protocol"`
		MimeType string `json:"mime_type"`
	} `json:"format"`
	Quality string `json:"quality"`
}, trackID int64) []DownloadResult {
	var results []DownloadResult

	for _, tc := range transcodings {
		if tc.URL == "" {
			continue
		}
		ext := "mp3"
		if strings.Contains(tc.Format.MimeType, "mpeg") {
			ext = "mp3"
		} else if strings.Contains(tc.Format.MimeType, "ogg") {
			ext = "ogg"
		}

		result := DownloadResult{
			URL:       tc.URL,
			Title:     fmt.Sprintf("%s - %s", username, title),
			MediaType: MediaTypeAudio,
			Quality:   QualityAudio,
			Extension: ext,
			Duration:  duration,
			Thumbnail: artworkURL,
			DirectURL: true,
			Filename:  fmt.Sprintf("%s_%s.%s", sanitizeFilename(username), sanitizeFilename(title), ext),
			Metadata: map[string]string{
				"service":   "soundcloud",
				"track_id":  fmt.Sprintf("%d", trackID),
				"author":    username,
				"quality":   tc.Quality,
				"protocol":  tc.Format.Protocol,
				"mime_type": tc.Format.MimeType,
			},
		}
		results = append(results, result)
	}

	return results
}

func (s *SoundCloudService) SupportsQuality() []Quality {
	return []Quality{QualityAudio}
}

func (s *SoundCloudService) SupportsMediaType() []MediaType {
	return []MediaType{MediaTypeAudio}
}
