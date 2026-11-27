package downloader

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
)

type TikTokService struct {
	*BaseService
}

func NewTikTokService(client *HTTPClient) *TikTokService {
	return &TikTokService{
		BaseService: NewBaseService("tiktok", []string{
			`https?://(?:www\.)?tiktok\.com/@[A-Za-z0-9_.]+/video/(\d+)`,
			`https?://(?:www\.)?tiktok\.com/t/([A-Za-z0-9]+)`,
			`https?://vm\.tiktok\.com/([A-Za-z0-9]+)`,
			`https?://(?:www\.)?tiktok\.com/video/(\d+)`,
		}, client),
	}
}

func (s *TikTokService) Extract(ctx context.Context, urlStr string) ([]DownloadResult, error) {
	urlStr = s.cleanURL(urlStr)

	videoID := s.extractVideoID(urlStr)
	if videoID == "" {
		return nil, fmt.Errorf("could not extract video ID")
	}

	apiURL := fmt.Sprintf("https://www.tiktok.com/api/video/detail/?itemId=%s", videoID)
	req := s.newRequest(ctx, "GET", apiURL)
	req.Header.Set("Referer", "https://www.tiktok.com/")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var data struct {
		StatusCode int `json:"status_code"`
		ItemInfo   struct {
			ItemStruct struct {
				ID         string `json:"id"`
				Desc       string `json:"desc"`
				CreateTime int64  `json:"createTime"`
				Video      struct {
					ID           string `json:"id"`
					Height       int    `json:"height"`
					Width        int    `json:"width"`
					Duration     int    `json:"duration"`
					Cover        string `json:"cover"`
					DynamicCover string `json:"dynamicCover"`
					PlayAddr     string `json:"playAddr"`
					DownloadAddr string `json:"downloadAddr"`
					Bitrate      int    `json:"bitrate"`
					Format       string `json:"format"`
				} `json:"video"`
				Music struct {
					ID          string `json:"id"`
					Title       string `json:"title"`
					PlayURL     string `json:"playUrl"`
					CoverMedium string `json:"coverMedium"`
				} `json:"music"`
				Author struct {
					ID       string `json:"id"`
					UniqueID string `json:"uniqueId"`
					Nickname string `json:"nickname"`
					Avatar   string `json:"avatarLarger"`
				} `json:"author"`
			} `json:"itemStruct"`
		} `json:"itemInfo"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}

	if data.StatusCode != 0 {
		return nil, fmt.Errorf("tiktok api error: %d", data.StatusCode)
	}

	item := data.ItemInfo.ItemStruct
	title := sanitizeFilename(item.Desc)
	if title == "" {
		title = fmt.Sprintf("tiktok_%s", item.ID)
	}

	video := item.Video
	videoURL := video.DownloadAddr
	if videoURL == "" {
		videoURL = video.PlayAddr
	}

	thumbnail := video.Cover
	if thumbnail == "" {
		thumbnail = video.DynamicCover
	}

	result := DownloadResult{
		URL:       videoURL,
		Title:     title,
		MediaType: MediaTypeVideo,
		Quality:   s.parseQuality(video.Height),
		Extension: "mp4",
		Duration:  video.Duration,
		Thumbnail: thumbnail,
		DirectURL: true,
		Filename:  fmt.Sprintf("%s.mp4", sanitizeFilename(title)),
		Metadata: map[string]string{
			"service":     "tiktok",
			"video_id":    videoID,
			"author":      item.Author.UniqueID,
			"author_name": item.Author.Nickname,
		},
	}

	if video.Width > 0 && video.Height > 0 {
		result.Metadata["width"] = fmt.Sprintf("%d", video.Width)
		result.Metadata["height"] = fmt.Sprintf("%d", video.Height)
	}

	if item.Music.ID != "" {
		audioResult := DownloadResult{
			URL:       item.Music.PlayURL,
			Title:     fmt.Sprintf("%s - %s", item.Author.Nickname, item.Music.Title),
			MediaType: MediaTypeAudio,
			Quality:   QualityAudio,
			Extension: "mp3",
			Duration:  video.Duration,
			Thumbnail: item.Music.CoverMedium,
			DirectURL: true,
			Filename:  fmt.Sprintf("%s_%s.mp3", sanitizeFilename(item.Author.Nickname), sanitizeFilename(item.Music.Title)),
			Metadata: map[string]string{
				"service":  "tiktok",
				"music_id": item.Music.ID,
			},
		}
		return []DownloadResult{result, audioResult}, nil
	}

	return []DownloadResult{result}, nil
}

func (s *TikTokService) extractVideoID(urlStr string) string {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`/video/(\d+)`),
		regexp.MustCompile(`vm\.tiktok\.com/([A-Za-z0-9]+)`),
		regexp.MustCompile(`/t/([A-Za-z0-9]+)`),
	}

	for _, p := range patterns {
		matches := p.FindStringSubmatch(urlStr)
		if len(matches) > 1 {
			return matches[1]
		}
	}
	return ""
}

func (s *TikTokService) parseQuality(height int) Quality {
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

func (s *TikTokService) SupportsQuality() []Quality {
	return []Quality{QualityBest, Quality1080p, Quality720p, Quality480p}
}

func (s *TikTokService) SupportsMediaType() []MediaType {
	return []MediaType{MediaTypeVideo, MediaTypeAudio}
}
