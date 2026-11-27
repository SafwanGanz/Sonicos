package downloader

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
)

type TwitterService struct {
	*BaseService
}

func NewTwitterService(client *HTTPClient) *TwitterService {
	return &TwitterService{
		BaseService: NewBaseService("twitter", []string{
			`https?://(?:www\.)?twitter\.com/[A-Za-z0-9_]+/status/(\d+)`,
			`https?://(?:www\.)?x\.com/[A-Za-z0-9_]+/status/(\d+)`,
			`https?://(?:mobile\.)?twitter\.com/[A-Za-z0-9_]+/status/(\d+)`,
		}, client),
	}
}

func (s *TwitterService) Extract(ctx context.Context, urlStr string) ([]DownloadResult, error) {
	urlStr = s.cleanURL(urlStr)
	tweetID := s.extractTweetID(urlStr)
	if tweetID == "" {
		return nil, fmt.Errorf("could not extract tweet ID")
	}

	apiURL := fmt.Sprintf("https://api.twitter.com/1.1/statuses/show.json?id=%s&tweet_mode=extended", tweetID)
	req := s.newRequest(ctx, "GET", apiURL)
	req.Header.Set("Authorization", "Bearer AAAAAAAAAAAAAAAAAAAAANRILgAAAAAAnNwIzUejRCOuH5E6I8xnZz4puTs%3D1Zv7ttfk8LF81IUq16cHjhLTvJu4FA33AGWWjCpTnA")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var data struct {
		ID       string `json:"id_str"`
		FullText string `json:"full_text"`
		User     struct {
			ScreenName string `json:"screen_name"`
			Name       string `json:"name"`
		} `json:"user"`
		ExtendedEntities struct {
			Media []struct {
				ID        string `json:"id_str"`
				Type      string `json:"type"`
				VideoInfo struct {
					Variants []struct {
						URL         string `json:"url"`
						ContentType string `json:"content_type"`
						Bitrate     int    `json:"bitrate"`
					} `json:"variants"`
				} `json:"video_info"`
				MediaURLHTTPS string `json:"media_url_https"`
				URL           string `json:"url"`
			} `json:"media"`
		} `json:"extended_entities"`
		Entities struct {
			Media []struct {
				ID        string `json:"id_str"`
				Type      string `json:"type"`
				VideoInfo struct {
					Variants []struct {
						URL         string `json:"url"`
						ContentType string `json:"content_type"`
						Bitrate     int    `json:"bitrate"`
					} `json:"variants"`
				} `json:"video_info"`
				MediaURLHTTPS string `json:"media_url_https"`
				URL           string `json:"url"`
			} `json:"media"`
		} `json:"entities"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}

	var results []DownloadResult
	title := sanitizeFilename(data.FullText)
	if len(title) > 50 {
		title = title[:50]
	}

	mediaItems := data.ExtendedEntities.Media
	if len(mediaItems) == 0 {
		mediaItems = data.Entities.Media
	}

	for _, media := range mediaItems {
		if media.Type == "video" || media.Type == "animated_gif" {
			var bestVariant struct {
				URL         string
				Bitrate     int
				ContentType string
			}
			for _, v := range media.VideoInfo.Variants {
				if v.ContentType == "video/mp4" && v.Bitrate > bestVariant.Bitrate {
					bestVariant = struct {
						URL         string
						Bitrate     int
						ContentType string
					}{v.URL, v.Bitrate, v.ContentType}
				}
			}
			if bestVariant.URL == "" && len(media.VideoInfo.Variants) > 0 {
				bestVariant.URL = media.VideoInfo.Variants[0].URL
			}
			if bestVariant.URL != "" {
				result := DownloadResult{
					URL:       bestVariant.URL,
					Title:     fmt.Sprintf("%s - %s", data.User.ScreenName, title),
					MediaType: MediaTypeVideo,
					Quality:   s.parseBitrateQuality(bestVariant.Bitrate),
					Extension: "mp4",
					DirectURL: true,
					Filename:  fmt.Sprintf("%s_%s.mp4", data.User.ScreenName, tweetID),
					Metadata: map[string]string{
						"service":  "twitter",
						"tweet_id": tweetID,
						"author":   data.User.ScreenName,
					},
				}
				results = append(results, result)
			}
		} else if media.Type == "photo" {
			result := DownloadResult{
				URL:       media.MediaURLHTTPS + "?format=jpg&name=orig",
				Title:     fmt.Sprintf("%s - %s", data.User.ScreenName, title),
				MediaType: MediaTypeImage,
				Quality:   QualityBest,
				Extension: "jpg",
				DirectURL: true,
				Filename:  fmt.Sprintf("%s_%s.jpg", data.User.ScreenName, tweetID),
				Metadata: map[string]string{
					"service":  "twitter",
					"tweet_id": tweetID,
					"author":   data.User.ScreenName,
				},
			}
			results = append(results, result)
		}
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("no media found in tweet")
	}

	return results, nil
}

func (s *TwitterService) extractTweetID(urlStr string) string {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`/status/(\d+)`),
	}
	for _, p := range patterns {
		matches := p.FindStringSubmatch(urlStr)
		if len(matches) > 1 {
			return matches[1]
		}
	}
	return ""
}

func (s *TwitterService) parseBitrateQuality(bitrate int) Quality {
	switch {
	case bitrate >= 2000000:
		return Quality1080p
	case bitrate >= 1000000:
		return Quality720p
	case bitrate >= 500000:
		return Quality480p
	default:
		return Quality360p
	}
}

func (s *TwitterService) SupportsQuality() []Quality {
	return []Quality{QualityBest, Quality1080p, Quality720p, Quality480p}
}

func (s *TwitterService) SupportsMediaType() []MediaType {
	return []MediaType{MediaTypeVideo, MediaTypeImage}
}
