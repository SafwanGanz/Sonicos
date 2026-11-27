package downloader

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
)

type BilibiliService struct {
	*BaseService
}

func NewBilibiliService(client *HTTPClient) *BilibiliService {
	return &BilibiliService{
		BaseService: NewBaseService("bilibili", []string{
			`https?://(?:www\.)?bilibili\.com/video/([A-Za-z0-9]+)`,
			`https?://b23\.tv/([A-Za-z0-9]+)`,
		}, client),
	}
}

func (s *BilibiliService) Extract(ctx context.Context, urlStr string) ([]DownloadResult, error) {
	urlStr = s.cleanURL(urlStr)
	bvid := s.extractBVID(urlStr)
	if bvid == "" {
		return nil, fmt.Errorf("could not extract BVID")
	}

	apiURL := fmt.Sprintf("https://api.bilibili.com/x/web-interface/view?bvid=%s", bvid)
	req := s.newRequest(ctx, "GET", apiURL)
	req.Header.Set("Referer", "https://www.bilibili.com/")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var data struct {
		Code int `json:"code"`
		Data struct {
			BVID     string `json:"bvid"`
			Title    string `json:"title"`
			Duration int    `json:"duration"`
			Owner    struct {
				Name string `json:"name"`
			} `json:"owner"`
			Pic string `json:"pic"`
			CID int64  `json:"cid"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}

	if data.Code != 0 {
		return nil, fmt.Errorf("bilibili api error: %d", data.Code)
	}

	video := data.Data
	title := sanitizeFilename(video.Title)

	playURL := fmt.Sprintf("https://api.bilibili.com/x/player/playurl?bvid=%s&cid=%d&qn=116&fnval=16&fnver=0&fourk=1", bvid, video.CID)
	req2 := s.newRequest(ctx, "GET", playURL)
	req2.Header.Set("Referer", "https://www.bilibili.com/")

	resp2, err := s.client.Do(req2)
	if err != nil {
		return nil, err
	}
	defer resp2.Body.Close()

	var playData struct {
		Code int `json:"code"`
		Data struct {
			Quality int    `json:"quality"`
			Format  string `json:"format"`
			Durl    []struct {
				URL    string `json:"url"`
				Size   int64  `json:"size"`
				Length int    `json:"length"`
				Order  int    `json:"order"`
			} `json:"durl"`
			Dash struct {
				Duration int `json:"duration"`
				Video    []struct {
					ID        int      `json:"id"`
					BaseURL   string   `json:"baseUrl"`
					BackupURL []string `json:"backupUrl"`
					Width     int      `json:"width"`
					Height    int      `json:"height"`
					Codecs    string   `json:"codecs"`
				} `json:"video"`
				Audio []struct {
					ID        int      `json:"id"`
					BaseURL   string   `json:"baseUrl"`
					BackupURL []string `json:"backupUrl"`
					Codecs    string   `json:"codecs"`
				} `json:"audio"`
			} `json:"dash"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp2.Body).Decode(&playData); err != nil {
		return nil, err
	}

	if playData.Code != 0 {
		return nil, fmt.Errorf("playurl api error: %d", playData.Code)
	}

	var results []DownloadResult

	if len(playData.Data.Dash.Video) > 0 && len(playData.Data.Dash.Audio) > 0 {
		for _, v := range playData.Data.Dash.Video {
			if v.BaseURL == "" {
				continue
			}
			quality := s.parseQuality(v.ID, v.Height)
			result := DownloadResult{
				URL:       v.BaseURL,
				Title:     title,
				MediaType: MediaTypeVideo,
				Quality:   quality,
				Extension: "mp4",
				Duration:  playData.Data.Dash.Duration,
				Thumbnail: video.Pic,
				DirectURL: true,
				Filename:  fmt.Sprintf("%s_%dp.mp4", title, v.Height),
				Metadata: map[string]string{
					"service": "bilibili",
					"bvid":    bvid,
					"cid":     fmt.Sprintf("%d", video.CID),
					"author":  video.Owner.Name,
					"quality": fmt.Sprintf("%dp", v.Height),
					"codecs":  v.Codecs,
				},
			}
			if v.Width > 0 && v.Height > 0 {
				result.Metadata["width"] = fmt.Sprintf("%d", v.Width)
				result.Metadata["height"] = fmt.Sprintf("%d", v.Height)
			}
			results = append(results, result)
		}
	} else {
		for _, d := range playData.Data.Durl {
			if d.URL == "" {
				continue
			}
			result := DownloadResult{
				URL:       d.URL,
				Title:     title,
				MediaType: MediaTypeVideo,
				Quality:   QualityBest,
				Extension: "flv",
				Duration:  d.Length,
				Thumbnail: video.Pic,
				DirectURL: true,
				Filename:  fmt.Sprintf("%s.flv", title),
				Metadata: map[string]string{
					"service": "bilibili",
					"bvid":    bvid,
					"author":  video.Owner.Name,
				},
			}
			results = append(results, result)
		}
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("no video sources found")
	}

	return results, nil
}

func (s *BilibiliService) extractBVID(urlStr string) string {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`/video/([A-Za-z0-9]+)`),
		regexp.MustCompile(`b23\.tv/([A-Za-z0-9]+)`),
	}
	for _, p := range patterns {
		matches := p.FindStringSubmatch(urlStr)
		if len(matches) > 1 {
			return matches[1]
		}
	}
	return ""
}

func (s *BilibiliService) parseQuality(id, height int) Quality {
	if height >= 1080 {
		return Quality1080p
	}
	if height >= 720 {
		return Quality720p
	}
	if height >= 480 {
		return Quality480p
	}
	return Quality360p
}

func (s *BilibiliService) SupportsQuality() []Quality {
	return []Quality{QualityBest, Quality1080p, Quality720p, Quality480p}
}

func (s *BilibiliService) SupportsMediaType() []MediaType {
	return []MediaType{MediaTypeVideo}
}
