package downloader

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
)

type TumblrService struct {
	*BaseService
}

func NewTumblrService(client *HTTPClient) *TumblrService {
	return &TumblrService{
		BaseService: NewBaseService("tumblr", []string{
			`https?://([A-Za-z0-9_-]+)\.tumblr\.com/post/(\d+)`,
			`https?://(?:www\.)?tumblr\.com/([A-Za-z0-9_-]+)/(\d+)`,
		}, client),
	}
}

func (s *TumblrService) Extract(ctx context.Context, urlStr string) ([]DownloadResult, error) {
	urlStr = s.cleanURL(urlStr)
	blogName, postID := s.extractInfo(urlStr)
	if blogName == "" || postID == "" {
		return nil, fmt.Errorf("could not extract blog name or post ID")
	}

	apiURL := fmt.Sprintf("https://api.tumblr.com/v2/blog/%s.tumblr.com/posts?id=%s", blogName, postID)
	req := s.newRequest(ctx, "GET", apiURL)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var data struct {
		Response struct {
			Posts []struct {
				ID        int64  `json:"id"`
				Type      string `json:"type"`
				Caption   string `json:"caption"`
				BlogName  string `json:"blog_name"`
				Photos    []struct {
					OriginalSize struct {
						URL    string `json:"url"`
						Width  int    `json:"width"`
						Height int    `json:"height"`
					} `json:"original_size"`
				} `json:"photos"`
				VideoPlayer string `json:"video_player"`
				VideoURL    string `json:"video_url"`
				VideoType   string `json:"video_type"`
			} `json:"posts"`
		} `json:"response"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}

	if len(data.Response.Posts) == 0 {
		return nil, fmt.Errorf("post not found")
	}

	post := data.Response.Posts[0]
	title := sanitizeFilename(post.Caption)
	if len(title) > 100 {
		title = title[:100]
	}

	var results []DownloadResult

	if post.Type == "video" && post.VideoURL != "" {
		result := DownloadResult{
			URL:       post.VideoURL,
			Title:     title,
			MediaType: MediaTypeVideo,
			Quality:   QualityBest,
			Extension: "mp4",
			DirectURL: true,
			Filename:  fmt.Sprintf("%s.mp4", title),
			Metadata: map[string]string{
				"service":   "tumblr",
				"post_id":   fmt.Sprintf("%d", post.ID),
				"blog":      post.BlogName,
				"type":      "video",
			},
		}
		results = append(results, result)
	}

	for _, photo := range post.Photos {
		if photo.OriginalSize.URL != "" {
			result := DownloadResult{
				URL:        photo.OriginalSize.URL,
				Title:      title,
				MediaType:  MediaTypeImage,
				Quality:    QualityBest,
				Extension:  "jpg",
				DirectURL:  true,
				Filename:   fmt.Sprintf("%s.jpg", title),
				Metadata: map[string]string{
					"service":  "tumblr",
					"post_id":  fmt.Sprintf("%d", post.ID),
					"blog":     post.BlogName,
					"type":     "image",
				},
			}
			results = append(results, result)
		}
	}

	if len(results) == 0 && post.VideoPlayer != "" {
		result := DownloadResult{
			URL:       post.VideoPlayer,
			Title:     title,
			MediaType: MediaTypeVideo,
			Quality:   QualityBest,
			Extension: "html",
			DirectURL: true,
			Filename:  fmt.Sprintf("%s.html", title),
			Metadata: map[string]string{
				"service":  "tumblr",
				"post_id":  fmt.Sprintf("%d", post.ID),
				"blog":     post.BlogName,
				"type":     "player",
			},
		}
		results = append(results, result)
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("no media found in post")
	}

	return results, nil
}

func (s *TumblrService) extractInfo(urlStr string) (string, string) {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`([A-Za-z0-9_-]+)\.tumblr\.com/post/(\d+)`),
		regexp.MustCompile(`tumblr\.com/([A-Za-z0-9_-]+)/(\d+)`),
	}
	for _, p := range patterns {
		matches := p.FindStringSubmatch(urlStr)
		if len(matches) > 2 {
			return matches[1], matches[2]
		}
	}
	return "", ""
}

func (s *TumblrService) SupportsQuality() []Quality {
	return []Quality{QualityBest}
}

func (s *TumblrService) SupportsMediaType() []MediaType {
	return []MediaType{MediaTypeVideo, MediaTypeImage}
}