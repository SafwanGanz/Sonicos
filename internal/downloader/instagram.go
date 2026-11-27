package downloader

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
)

type InstagramService struct {
	*BaseService
}

func NewInstagramService(client *HTTPClient) *InstagramService {
	return &InstagramService{
		BaseService: NewBaseService("instagram", []string{
			`https?://(?:www\.)?instagram\.com/(?:p|reel|reels|tv|stories)/([A-Za-z0-9_-]+)`,
			`https?://(?:www\.)?instagram\.com/([A-Za-z0-9_.]+)/?\?.*`,
		}, client),
	}
}

func (s *InstagramService) Extract(ctx context.Context, urlStr string) ([]DownloadResult, error) {
	urlStr = s.cleanURL(urlStr)
	shortcode := s.extractShortcode(urlStr)
	if shortcode == "" {
		return nil, fmt.Errorf("could not extract shortcode from URL")
	}

	apiURL := fmt.Sprintf("https://www.instagram.com/api/v1/media/%s/info/", shortcode)
	req := s.newRequest(ctx, "GET", apiURL)
	req.Header.Set("X-IG-App-ID", "936619743392459")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")

	resp, err := s.client.Do(req)
	if err != nil {
		return s.tryGraphQL(ctx, shortcode)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return s.tryGraphQL(ctx, shortcode)
	}

	var data struct {
		Items []struct {
			ID            string `json:"id"`
			Code          string `json:"code"`
			MediaType     int    `json:"media_type"`
			VideoVersions []struct {
				URL    string `json:"url"`
				Width  int    `json:"width"`
				Height int    `json:"height"`
				Type   int    `json:"type"`
			} `json:"video_versions"`
			ImageVersions2 struct {
				Candidates []struct {
					URL    string `json:"url"`
					Width  int    `json:"width"`
					Height int    `json:"height"`
				} `json:"candidates"`
			} `json:"image_versions2"`
			VideoDuration float64 `json:"video_duration"`
			Caption       struct {
				Text string `json:"text"`
			} `json:"caption"`
			CarouselMedia []struct {
				MediaType     int `json:"media_type"`
				VideoVersions []struct {
					URL    string `json:"url"`
					Width  int    `json:"width"`
					Height int    `json:"height"`
				} `json:"video_versions"`
				ImageVersions2 struct {
					Candidates []struct {
						URL    string `json:"url"`
						Width  int    `json:"width"`
						Height int    `json:"height"`
					} `json:"candidates"`
				} `json:"image_versions2"`
			} `json:"carousel_media"`
			ProductType string `json:"product_type"`
		} `json:"items"`
		Status string `json:"status"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return s.tryGraphQL(ctx, shortcode)
	}

	if data.Status != "ok" || len(data.Items) == 0 {
		return s.tryGraphQL(ctx, shortcode)
	}

	return s.parseItems(data.Items, shortcode)
}

func (s *InstagramService) tryGraphQL(ctx context.Context, shortcode string) ([]DownloadResult, error) {
	variables := map[string]interface{}{
		"shortcode":             shortcode,
		"child_comment_count":   3,
		"fetch_comment_count":   40,
		"parent_comment_count":  24,
		"has_threaded_comments": true,
	}

	variablesJSON, _ := json.Marshal(variables)
	queryHash := "b3055c01b4b222b8a47dc12b090e4e64"
	apiURL := fmt.Sprintf("https://www.instagram.com/graphql/query/?query_hash=%s&variables=%s", queryHash, url.QueryEscape(string(variablesJSON)))

	req := s.newRequest(ctx, "GET", apiURL)
	req.Header.Set("X-IG-App-ID", "936619743392459")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("graphql request failed: %w", err)
	}
	defer resp.Body.Close()

	var data struct {
		Data struct {
			ShortcodeMedia struct {
				ID            string  `json:"id"`
				Shortcode     string  `json:"shortcode"`
				Typename      string  `json:"__typename"`
				IsVideo       bool    `json:"is_video"`
				VideoURL      string  `json:"video_url"`
				DisplayURL    string  `json:"display_url"`
				VideoDuration float64 `json:"video_duration"`
				Dimensions    struct {
					Width  int `json:"width"`
					Height int `json:"height"`
				} `json:"dimensions"`
				EdgeSidecarToChildren struct {
					Edges []struct {
						Node struct {
							ID            string  `json:"id"`
							Typename      string  `json:"__typename"`
							IsVideo       bool    `json:"is_video"`
							VideoURL      string  `json:"video_url"`
							DisplayURL    string  `json:"display_url"`
							VideoDuration float64 `json:"video_duration"`
							Dimensions    struct {
								Width  int `json:"width"`
								Height int `json:"height"`
							} `json:"dimensions"`
						} `json:"node"`
					} `json:"edges"`
				} `json:"edge_sidecar_to_children"`
				Owner struct {
					Username string `json:"username"`
				} `json:"owner"`
				EdgeMediaToCaption struct {
					Edges []struct {
						Node struct {
							Text string `json:"text"`
						} `json:"node"`
					} `json:"edges"`
				} `json:"edge_media_to_caption"`
			} `json:"shortcode_media"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("graphql decode failed: %w", err)
	}

	media := data.Data.ShortcodeMedia
	var results []DownloadResult

	baseResult := DownloadResult{
		Title:     media.Owner.Username + "_" + media.Shortcode,
		DirectURL: true,
		Metadata:  map[string]string{"service": "instagram"},
	}

	if media.Typename == "GraphSidecar" {
		for _, edge := range media.EdgeSidecarToChildren.Edges {
			node := edge.Node
			result := baseResult
			if node.IsVideo {
				result.URL = node.VideoURL
				result.MediaType = MediaTypeVideo
				result.Quality = Quality720p
				result.Extension = "mp4"
				result.Duration = int(node.VideoDuration)
				result.Filename = fmt.Sprintf("%s_%s.mp4", media.Owner.Username, node.ID)
				result.Metadata["width"] = fmt.Sprintf("%d", node.Dimensions.Width)
				result.Metadata["height"] = fmt.Sprintf("%d", node.Dimensions.Height)
			} else {
				result.URL = node.DisplayURL
				result.MediaType = MediaTypeImage
				result.Quality = QualityBest
				result.Extension = "jpg"
				result.Filename = fmt.Sprintf("%s_%s.jpg", media.Owner.Username, node.ID)
				result.Metadata["width"] = fmt.Sprintf("%d", node.Dimensions.Width)
				result.Metadata["height"] = fmt.Sprintf("%d", node.Dimensions.Height)
			}
			results = append(results, result)
		}
	} else if media.IsVideo {
		baseResult.URL = media.VideoURL
		baseResult.MediaType = MediaTypeVideo
		baseResult.Quality = Quality720p
		baseResult.Extension = "mp4"
		baseResult.Duration = int(media.VideoDuration)
		baseResult.Filename = fmt.Sprintf("%s_%s.mp4", media.Owner.Username, media.Shortcode)
		baseResult.Metadata["width"] = fmt.Sprintf("%d", media.Dimensions.Width)
		baseResult.Metadata["height"] = fmt.Sprintf("%d", media.Dimensions.Height)
		results = append(results, baseResult)
	} else {
		baseResult.URL = media.DisplayURL
		baseResult.MediaType = MediaTypeImage
		baseResult.Quality = QualityBest
		baseResult.Extension = "jpg"
		baseResult.Filename = fmt.Sprintf("%s_%s.jpg", media.Owner.Username, media.Shortcode)
		baseResult.Metadata["width"] = fmt.Sprintf("%d", media.Dimensions.Width)
		baseResult.Metadata["height"] = fmt.Sprintf("%d", media.Dimensions.Height)
		results = append(results, baseResult)
	}

	return results, nil
}

func (s *InstagramService) parseItems(items []struct {
	ID            string `json:"id"`
	Code          string `json:"code"`
	MediaType     int    `json:"media_type"`
	VideoVersions []struct {
		URL    string `json:"url"`
		Width  int    `json:"width"`
		Height int    `json:"height"`
		Type   int    `json:"type"`
	} `json:"video_versions"`
	ImageVersions2 struct {
		Candidates []struct {
			URL    string `json:"url"`
			Width  int    `json:"width"`
			Height int    `json:"height"`
		} `json:"candidates"`
	} `json:"image_versions2"`
	VideoDuration float64 `json:"video_duration"`
	Caption       struct {
		Text string `json:"text"`
	} `json:"caption"`
	CarouselMedia []struct {
		MediaType     int `json:"media_type"`
		VideoVersions []struct {
			URL    string `json:"url"`
			Width  int    `json:"width"`
			Height int    `json:"height"`
		} `json:"video_versions"`
		ImageVersions2 struct {
			Candidates []struct {
				URL    string `json:"url"`
				Width  int    `json:"width"`
				Height int    `json:"height"`
			} `json:"candidates"`
		} `json:"image_versions2"`
	} `json:"carousel_media"`
	ProductType string `json:"product_type"`
}, shortcode string) ([]DownloadResult, error) {
	var results []DownloadResult

	for _, item := range items {
		if len(item.CarouselMedia) > 0 {
			for i, media := range item.CarouselMedia {
				result := DownloadResult{
					Title:     fmt.Sprintf("%s_%d", shortcode, i),
					DirectURL: true,
					Metadata:  map[string]string{"service": "instagram"},
				}
				if media.MediaType == 2 {
					if len(media.VideoVersions) > 0 {
						v := media.VideoVersions[0]
						result.URL = v.URL
						result.MediaType = MediaTypeVideo
						result.Quality = Quality720p
						result.Extension = "mp4"
						result.Metadata["width"] = fmt.Sprintf("%d", v.Width)
						result.Metadata["height"] = fmt.Sprintf("%d", v.Height)
						result.Filename = fmt.Sprintf("%s_%d.mp4", shortcode, i)
					}
				} else {
					if len(media.ImageVersions2.Candidates) > 0 {
						img := media.ImageVersions2.Candidates[0]
						result.URL = img.URL
						result.MediaType = MediaTypeImage
						result.Quality = QualityBest
						result.Extension = "jpg"
						result.Metadata["width"] = fmt.Sprintf("%d", img.Width)
						result.Metadata["height"] = fmt.Sprintf("%d", img.Height)
						result.Filename = fmt.Sprintf("%s_%d.jpg", shortcode, i)
					}
				}
				results = append(results, result)
			}
		} else if item.MediaType == 2 {
			result := DownloadResult{
				Title:     shortcode,
				DirectURL: true,
				Metadata:  map[string]string{"service": "instagram"},
			}
			if len(item.VideoVersions) > 0 {
				v := item.VideoVersions[0]
				result.URL = v.URL
				result.MediaType = MediaTypeVideo
				result.Quality = Quality720p
				result.Extension = "mp4"
				result.Duration = int(item.VideoDuration)
				result.Metadata["width"] = fmt.Sprintf("%d", v.Width)
				result.Metadata["height"] = fmt.Sprintf("%d", v.Height)
				result.Filename = fmt.Sprintf("%s.mp4", shortcode)
			}
			results = append(results, result)
		} else {
			result := DownloadResult{
				Title:     shortcode,
				DirectURL: true,
				Metadata:  map[string]string{"service": "instagram"},
			}
			if len(item.ImageVersions2.Candidates) > 0 {
				img := item.ImageVersions2.Candidates[0]
				result.URL = img.URL
				result.MediaType = MediaTypeImage
				result.Quality = QualityBest
				result.Extension = "jpg"
				result.Metadata["width"] = fmt.Sprintf("%d", img.Width)
				result.Metadata["height"] = fmt.Sprintf("%d", img.Height)
				result.Filename = fmt.Sprintf("%s.jpg", shortcode)
			}
			results = append(results, result)
		}
	}

	return results, nil
}

func (s *InstagramService) extractShortcode(urlStr string) string {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`/([A-Za-z0-9_-]{5,})`),
		regexp.MustCompile(`/reel/([A-Za-z0-9_-]+)`),
		regexp.MustCompile(`/reels/([A-Za-z0-9_-]+)`),
		regexp.MustCompile(`/p/([A-Za-z0-9_-]+)`),
		regexp.MustCompile(`/tv/([A-Za-z0-9_-]+)`),
	}

	for _, p := range patterns {
		matches := p.FindStringSubmatch(urlStr)
		if len(matches) > 1 {
			return matches[1]
		}
	}
	return ""
}

func (s *InstagramService) SupportsQuality() []Quality {
	return []Quality{QualityBest, Quality1080p, Quality720p}
}

func (s *InstagramService) SupportsMediaType() []MediaType {
	return []MediaType{MediaTypeVideo, MediaTypeImage, MediaTypeMixed}
}
