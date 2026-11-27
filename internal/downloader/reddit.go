package downloader

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
)

type RedditService struct {
	*BaseService
}

func NewRedditService(client *HTTPClient) *RedditService {
	return &RedditService{
		BaseService: NewBaseService("reddit", []string{
			`https?://(?:www\.)?reddit\.com/r/[A-Za-z0-9_]+/comments/([A-Za-z0-9]+)`,
			`https?://(?:www\.)?redd\.it/([A-Za-z0-9]+)`,
			`https?://(?:old\.)?reddit\.com/r/[A-Za-z0-9_]+/comments/([A-Za-z0-9]+)`,
		}, client),
	}
}

func (s *RedditService) Extract(ctx context.Context, urlStr string) ([]DownloadResult, error) {
	urlStr = s.cleanURL(urlStr)
	postID := s.extractPostID(urlStr)
	if postID == "" {
		return nil, fmt.Errorf("could not extract post ID")
	}

	apiURL := fmt.Sprintf("https://www.reddit.com/comments/%s/.json", postID)
	req := s.newRequest(ctx, "GET", apiURL)
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; RedditDownloader/1.0)")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var data []struct {
		Data struct {
			Children []struct {
				Data struct {
					ID          string `json:"id"`
					Title       string `json:"title"`
					Author      string `json:"author"`
					Subreddit   string `json:"subreddit"`
					URL         string `json:"url"`
					IsVideo     bool   `json:"is_video"`
					SecureMedia struct {
						RedditVideo struct {
							FallbackURL string `json:"fallback_url"`
							Height      int    `json:"height"`
							Width       int    `json:"width"`
							Duration    int    `json:"duration"`
							DashURL     string `json:"dash_url"`
							HLSURL      string `json:"hls_url"`
						} `json:"reddit_video"`
					} `json:"secure_media"`
					MediaMetadata map[string]struct {
						Status string `json:"status"`
						E      string `json:"e"`
						S      struct {
							U   string `json:"u"`
							GIF string `json:"gif"`
							MP4 string `json:"mp4"`
						} `json:"s"`
						P []struct {
							U string `json:"u"`
						} `json:"p"`
					} `json:"media_metadata"`
				} `json:"data"`
			} `json:"children"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}

	if len(data) == 0 || len(data[0].Data.Children) == 0 {
		return nil, fmt.Errorf("no post data found")
	}

	post := data[0].Data.Children[0].Data
	title := sanitizeFilename(post.Title)
	if len(title) > 100 {
		title = title[:100]
	}

	var results []DownloadResult

	if post.IsVideo && post.SecureMedia.RedditVideo.FallbackURL != "" {
		video := post.SecureMedia.RedditVideo
		result := DownloadResult{
			URL:       video.FallbackURL,
			Title:     title,
			MediaType: MediaTypeVideo,
			Quality:   s.parseQuality(video.Height),
			Extension: "mp4",
			Duration:  video.Duration,
			DirectURL: true,
			Filename:  fmt.Sprintf("%s.mp4", sanitizeFilename(title)),
			Metadata: map[string]string{
				"service":   "reddit",
				"post_id":   post.ID,
				"subreddit": post.Subreddit,
				"author":    post.Author,
			},
		}
		if video.Width > 0 && video.Height > 0 {
			result.Metadata["width"] = fmt.Sprintf("%d", video.Width)
			result.Metadata["height"] = fmt.Sprintf("%d", video.Height)
		}
		results = append(results, result)
	}

	for _, meta := range post.MediaMetadata {
		if meta.Status == "valid" {
			if meta.S.MP4 != "" {
				result := DownloadResult{
					URL:       meta.S.MP4,
					Title:     title,
					MediaType: MediaTypeVideo,
					Quality:   QualityBest,
					Extension: "mp4",
					DirectURL: true,
					Filename:  fmt.Sprintf("%s_gallery.mp4", sanitizeFilename(title)),
					Metadata: map[string]string{
						"service":   "reddit",
						"post_id":   post.ID,
						"subreddit": post.Subreddit,
						"author":    post.Author,
					},
				}
				results = append(results, result)
			} else if meta.S.GIF != "" {
				result := DownloadResult{
					URL:       meta.S.GIF,
					Title:     title,
					MediaType: MediaTypeImage,
					Quality:   QualityBest,
					Extension: "gif",
					DirectURL: true,
					Filename:  fmt.Sprintf("%s.gif", sanitizeFilename(title)),
					Metadata: map[string]string{
						"service":   "reddit",
						"post_id":   post.ID,
						"subreddit": post.Subreddit,
						"author":    post.Author,
					},
				}
				results = append(results, result)
			} else if len(meta.P) > 0 {
				result := DownloadResult{
					URL:       meta.P[0].U,
					Title:     title,
					MediaType: MediaTypeImage,
					Quality:   QualityBest,
					Extension: "jpg",
					DirectURL: true,
					Filename:  fmt.Sprintf("%s.jpg", sanitizeFilename(title)),
					Metadata: map[string]string{
						"service":   "reddit",
						"post_id":   post.ID,
						"subreddit": post.Subreddit,
						"author":    post.Author,
					},
				}
				results = append(results, result)
			}
		}
	}

	if len(results) == 0 && post.URL != "" {
		result := DownloadResult{
			URL:       post.URL,
			Title:     title,
			MediaType: MediaTypeMixed,
			Quality:   QualityBest,
			DirectURL: true,
			Filename:  fmt.Sprintf("%s", sanitizeFilename(title)),
			Metadata: map[string]string{
				"service":   "reddit",
				"post_id":   post.ID,
				"subreddit": post.Subreddit,
				"author":    post.Author,
			},
		}
		results = append(results, result)
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("no media found in post")
	}

	return results, nil
}

func (s *RedditService) extractPostID(urlStr string) string {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`/comments/([A-Za-z0-9]+)`),
		regexp.MustCompile(`redd\.it/([A-Za-z0-9]+)`),
	}
	for _, p := range patterns {
		matches := p.FindStringSubmatch(urlStr)
		if len(matches) > 1 {
			return matches[1]
		}
	}
	return ""
}

func (s *RedditService) parseQuality(height int) Quality {
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

func (s *RedditService) SupportsQuality() []Quality {
	return []Quality{QualityBest, Quality1080p, Quality720p, Quality480p}
}

func (s *RedditService) SupportsMediaType() []MediaType {
	return []MediaType{MediaTypeVideo, MediaTypeImage}
}
