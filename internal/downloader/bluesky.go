package downloader

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
)

type BlueskyService struct {
	*BaseService
}

func NewBlueskyService(client *HTTPClient) *BlueskyService {
	return &BlueskyService{
		BaseService: NewBaseService("bluesky", []string{
			`https?://(?:www\.)?bsky\.app/profile/[A-Za-z0-9._-]+/post/([A-Za-z0-9]+)`,
			`https?://(?:www\.)?bsky\.app/profile/did:plc:[A-Za-z0-9]+/post/([A-Za-z0-9]+)`,
		}, client),
	}
}

func (s *BlueskyService) Extract(ctx context.Context, urlStr string) ([]DownloadResult, error) {
	urlStr = s.cleanURL(urlStr)
	postID := s.extractPostID(urlStr)
	if postID == "" {
		return nil, fmt.Errorf("could not extract post ID")
	}

	apiURL := fmt.Sprintf("https://public.api.bsky.app/xrpc/app.bsky.feed.getPosts?uris=at://%%2F%%2Fpost%%2F%s", postID)
	req := s.newRequest(ctx, "GET", apiURL)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var data struct {
		Posts []struct {
			URI   string `json:"uri"`
			CID   string `json:"cid"`
			Author struct {
				Handle string `json:"handle"`
			} `json:"author"`
			Record struct {
				Text string `json:"text"`
				Embed struct {
					Images []struct {
						Image struct {
							Ref struct {
								Link string `json:"$link"`
							} `json:"ref"`
						} `json:"image"`
						Alt string `json:"alt"`
					} `json:"images"`
					Video struct {
						Playlist string `json:"playlist"`
					} `json:"video"`
				} `json:"embed"`
			} `json:"record"`
		} `json:"posts"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}

	if len(data.Posts) == 0 {
		return nil, fmt.Errorf("post not found")
	}

	post := data.Posts[0]
	title := sanitizeFilename(post.Record.Text)
	if len(title) > 100 {
		title = title[:100]
	}

	var results []DownloadResult

	if post.Record.Embed.Video.Playlist != "" {
		result := DownloadResult{
			URL:        post.Record.Embed.Video.Playlist,
			Title:      title,
			MediaType:  MediaTypeVideo,
			Quality:    QualityBest,
			Extension:  "m3u8",
			DirectURL:  true,
			Filename:   fmt.Sprintf("%s.m3u8", title),
			Metadata: map[string]string{
				"service":   "bluesky",
				"post_id":   postID,
				"author":    post.Author.Handle,
				"type":      "video",
			},
		}
		results = append(results, result)
	}

	for _, img := range post.Record.Embed.Images {
		if img.Image.Ref.Link != "" {
			result := DownloadResult{
				URL:       fmt.Sprintf("https://cdn.bsky.app/img/feed_fullsize/plain/%s@jpeg", img.Image.Ref.Link),
				Title:     title,
				MediaType: MediaTypeImage,
				Quality:   QualityBest,
				Extension: "jpg",
				DirectURL: true,
				Filename:  fmt.Sprintf("%s.jpg", title),
				Metadata: map[string]string{
					"service":  "bluesky",
					"post_id":  postID,
					"author":   post.Author.Handle,
					"type":     "image",
				},
			}
			results = append(results, result)
		}
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("no media found in post")
	}

	return results, nil
}

func (s *BlueskyService) extractPostID(urlStr string) string {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`/post/([A-Za-z0-9]+)`),
	}
	for _, p := range patterns {
		matches := p.FindStringSubmatch(urlStr)
		if len(matches) > 1 {
			return matches[1]
		}
	}
	return ""
}

func (s *BlueskyService) SupportsQuality() []Quality {
	return []Quality{QualityBest}
}

func (s *BlueskyService) SupportsMediaType() []MediaType {
	return []MediaType{MediaTypeVideo, MediaTypeImage}
}