package downloader

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

type YouTubeService struct {
	*BaseService
}

func NewYouTubeService(client *HTTPClient) *YouTubeService {
	return &YouTubeService{
		BaseService: NewBaseService("youtube", []string{
			`https?://(?:www\.)?youtube\.com/watch\?v=([A-Za-z0-9_-]+)`,
			`https?://(?:www\.)?youtube\.com/shorts/([A-Za-z0-9_-]+)`,
			`https?://youtu\.be/([A-Za-z0-9_-]+)`,
			`https?://(?:www\.)?youtube\.com/embed/([A-Za-z0-9_-]+)`,
			`https?://(?:www\.)?youtube\.com/v/([A-Za-z0-9_-]+)`,
			`https?://music\.youtube\.com/watch\?v=([A-Za-z0-9_-]+)`,
		}, client),
	}
}

type YouTubeData struct {
	StreamingData struct {
		Formats         []Format `json:"formats"`
		AdaptiveFormats []Format `json:"adaptiveFormats"`
		DASHManifestURL string   `json:"dashManifestUrl"`
		HLSManifestURL  string   `json:"hlsManifestUrl"`
	} `json:"streamingData"`
	VideoDetails struct {
		VideoID       string `json:"videoId"`
		Title         string `json:"title"`
		LengthSeconds string `json:"lengthSeconds"`
		Author        string `json:"author"`
		ViewCount     string `json:"viewCount"`
		Thumbnails    []struct {
			URL    string `json:"url"`
			Width  int    `json:"width"`
			Height int    `json:"height"`
		} `json:"thumbnails"`
	} `json:"videoDetails"`
	PlayabilityStatus struct {
		Status string `json:"status"`
		Reason string `json:"reason,omitempty"`
	} `json:"playabilityStatus"`
}

type Format struct {
	URL              string `json:"url"`
	MimeType         string `json:"mimeType"`
	Bitrate          int    `json:"bitrate"`
	Width            int    `json:"width"`
	Height           int    `json:"height"`
	Quality          string `json:"quality"`
	QualityLabel     string `json:"qualityLabel"`
	ContentLength    string `json:"contentLength"`
	ApproxDurationMs string `json:"approxDurationMs"`
	AudioQuality     string `json:"audioQuality"`
	AudioChannels    int    `json:"audioChannels"`
	AudioSampleRate  string `json:"audioSampleRate"`
	InitRange        struct {
		Start string `json:"start"`
		End   string `json:"end"`
	} `json:"initRange"`
	IndexRange struct {
		Start string `json:"start"`
		End   string `json:"end"`
	} `json:"indexRange"`
	LastModified string `json:"lastModified"`
}

func (s *YouTubeService) Extract(ctx context.Context, urlStr string) ([]DownloadResult, error) {
	urlStr = s.cleanURL(urlStr)
	videoID := s.extractVideoID(urlStr)
	if videoID == "" {
		return nil, fmt.Errorf("could not extract video ID from URL")
	}

	results, err := s.tryInnertubeAPI(ctx, videoID)
	if err == nil && len(results) > 0 {
		return results, nil
	}

	return s.tryYTDLP(ctx, urlStr)
}

func (s *YouTubeService) tryInnertubeAPI(ctx context.Context, videoID string) ([]DownloadResult, error) {
	playerURL := "https://www.youtube.com/youtubei/v1/player?key=AIzaSyAO_FJ2SlqU8Q4STEHLGCilw_Y9_11qcW8"

	payload := map[string]interface{}{
		"context": map[string]interface{}{
			"client": map[string]interface{}{
				"clientName":    "WEB",
				"clientVersion": "2.20241113.00.00",
			},
		},
		"videoId": videoID,
		"playbackContext": map[string]interface{}{
			"contentPlaybackContext": map[string]interface{}{
				"html5Preference": "HTML5_PREF_WANTS",
			},
		},
		"contentCheckOk": true,
		"racyCheckOk":    true,
	}

	body, _ := json.Marshal(payload)
	req := s.newRequest(ctx, "POST", playerURL)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-YouTube-Client-Name", "1")
	req.Header.Set("X-YouTube-Client-Version", "2.20241113.00.00")
	req.Body = io.NopCloser(bytes.NewReader(body))

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	var data YouTubeData

	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}

	if data.PlayabilityStatus.Status != "OK" {
		return nil, fmt.Errorf("video not playable: %s", data.PlayabilityStatus.Reason)
	}

	return s.parseFormats(data, videoID)
}

func (s *YouTubeService) tryYTDLP(ctx context.Context, urlStr string) ([]DownloadResult, error) {
	tmpDir, err := os.MkdirTemp("", "ytdlp-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	cmd := exec.CommandContext(ctx, "yt-dlp",
		"-f", "bestvideo+bestaudio/best",
		"--no-playlist",
		"--no-warnings",
		"--ignore-errors",
		"-J",
		urlStr,
	)
	cmd.Dir = tmpDir

	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("yt-dlp failed: %w", err)
	}

	var info struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		Duration  int    `json:"duration"`
		Thumbnail string `json:"thumbnail"`
		Uploader  string `json:"uploader"`
		Formats   []struct {
			URL        string  `json:"url"`
			FormatID   string  `json:"format_id"`
			Ext        string  `json:"ext"`
			Protocol   string  `json:"protocol"`
			Vcodec     string  `json:"vcodec"`
			Acodec     string  `json:"acodec"`
			Width      int     `json:"width"`
			Height     int     `json:"height"`
			Filesize   int64   `json:"filesize"`
			TBR        float64 `json:"tbr"`
			FormatNote string  `json:"format_note"`
		} `json:"formats"`
	}

	if err := json.Unmarshal(output, &info); err != nil {
		return nil, fmt.Errorf("parse yt-dlp output: %w", err)
	}

	var results []DownloadResult
	for _, f := range info.Formats {
		if f.URL == "" || f.Protocol == "m3u8_native" || f.Protocol == "m3u8" {
			continue
		}
		mediaType := MediaTypeVideo
		if f.Vcodec == "none" && f.Acodec != "none" {
			mediaType = MediaTypeAudio
		}
		quality := QualityBest
		if f.Height >= 1080 {
			quality = Quality1080p
		} else if f.Height >= 720 {
			quality = Quality720p
		} else if f.Height >= 480 {
			quality = Quality480p
		} else if f.Height >= 360 {
			quality = Quality360p
		}
		if mediaType == MediaTypeAudio {
			quality = QualityAudio
		}

		result := DownloadResult{
			URL:       f.URL,
			Title:     info.Title,
			MediaType: mediaType,
			Quality:   quality,
			Extension: f.Ext,
			Duration:  info.Duration,
			Thumbnail: info.Thumbnail,
			Filesize:  f.Filesize,
			DirectURL: true,
			Filename:  fmt.Sprintf("%s.%s", sanitizeFilename(info.Title), f.Ext),
			Metadata: map[string]string{
				"service":   "youtube",
				"video_id":  info.ID,
				"author":    info.Uploader,
				"format_id": f.FormatID,
			},
		}
		if f.Width > 0 && f.Height > 0 {
			result.Metadata["width"] = fmt.Sprintf("%d", f.Width)
			result.Metadata["height"] = fmt.Sprintf("%d", f.Height)
		}
		results = append(results, result)
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("no formats found via yt-dlp")
	}

	return results, nil
}

func (s *YouTubeService) parseFormats(data YouTubeData, videoID string) ([]DownloadResult, error) {
	var results []DownloadResult

	allFormats := append(data.StreamingData.Formats, data.StreamingData.AdaptiveFormats...)

	videoFormats := make([]Format, 0)
	audioFormats := make([]Format, 0)

	for _, f := range allFormats {
		if f.URL == "" {
			continue
		}
		if strings.Contains(f.MimeType, "video/") && strings.Contains(f.MimeType, "audio/") {
			videoFormats = append(videoFormats, f)
		} else if strings.Contains(f.MimeType, "video/") {
			videoFormats = append(videoFormats, f)
		} else if strings.Contains(f.MimeType, "audio/") {
			audioFormats = append(audioFormats, f)
		}
	}

	thumbnail := ""
	if len(data.VideoDetails.Thumbnails) > 0 {
		thumbnail = data.VideoDetails.Thumbnails[len(data.VideoDetails.Thumbnails)-1].URL
	}

	duration := 0
	if data.VideoDetails.LengthSeconds != "" {
		fmt.Sscanf(data.VideoDetails.LengthSeconds, "%d", &duration)
	}

	title := sanitizeFilename(data.VideoDetails.Title)

	for _, f := range videoFormats {
		quality := s.parseQuality(f.QualityLabel, f.Height)
		ext := s.getExtension(f.MimeType)

		result := DownloadResult{
			URL:       f.URL,
			Title:     title,
			MediaType: MediaTypeVideo,
			Quality:   quality,
			Extension: ext,
			Duration:  duration,
			Thumbnail: thumbnail,
			Filesize:  s.parseInt64(f.ContentLength),
			DirectURL: true,
			Metadata: map[string]string{
				"service":       "youtube",
				"video_id":      videoID,
				"author":        data.VideoDetails.Author,
				"mime_type":     f.MimeType,
				"quality_label": f.QualityLabel,
			},
		}
		if f.Width > 0 && f.Height > 0 {
			result.Metadata["width"] = fmt.Sprintf("%d", f.Width)
			result.Metadata["height"] = fmt.Sprintf("%d", f.Height)
		}
		result.Filename = fmt.Sprintf("%s.%s", sanitizeFilename(title), ext)
		results = append(results, result)
	}

	for _, f := range audioFormats {
		ext := s.getExtension(f.MimeType)
		result := DownloadResult{
			URL:       f.URL,
			Title:     title + " (audio)",
			MediaType: MediaTypeAudio,
			Quality:   QualityAudio,
			Extension: ext,
			Duration:  duration,
			Thumbnail: thumbnail,
			Filesize:  s.parseInt64(f.ContentLength),
			DirectURL: true,
			Metadata: map[string]string{
				"service":   "youtube",
				"video_id":  videoID,
				"author":    data.VideoDetails.Author,
				"mime_type": f.MimeType,
			},
		}
		result.Filename = fmt.Sprintf("%s_audio.%s", sanitizeFilename(title), ext)
		results = append(results, result)
	}

	return results, nil
}

func (s *YouTubeService) parseQuality(label string, height int) Quality {
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
	return QualityBest
}

func (s *YouTubeService) getExtension(mimeType string) string {
	switch {
	case strings.Contains(mimeType, "mp4"):
		return "mp4"
	case strings.Contains(mimeType, "webm"):
		return "webm"
	case strings.Contains(mimeType, "ogg"):
		return "ogg"
	case strings.Contains(mimeType, "m4a"):
		return "m4a"
	default:
		return "mp4"
	}
}

func (s *YouTubeService) parseInt64(str string) int64 {
	var n int64
	fmt.Sscanf(str, "%d", &n)
	return n
}

func (s *YouTubeService) extractVideoID(urlStr string) string {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`[?&]v=([A-Za-z0-9_-]{11})`),
		regexp.MustCompile(`/shorts/([A-Za-z0-9_-]{11})`),
		regexp.MustCompile(`youtu\.be/([A-Za-z0-9_-]{11})`),
		regexp.MustCompile(`/embed/([A-Za-z0-9_-]{11})`),
		regexp.MustCompile(`/v/([A-Za-z0-9_-]{11})`),
	}

	for _, p := range patterns {
		matches := p.FindStringSubmatch(urlStr)
		if len(matches) > 1 {
			return matches[1]
		}
	}
	return ""
}

func (s *YouTubeService) SupportsQuality() []Quality {
	return []Quality{QualityBest, Quality1080p, Quality720p, Quality480p, Quality360p, QualityAudio}
}

func (s *YouTubeService) SupportsMediaType() []MediaType {
	return []MediaType{MediaTypeVideo, MediaTypeAudio}
}

func sanitizeFilename(s string) string {
	re := regexp.MustCompile(`[<>:"/\|?*]`)
	s = re.ReplaceAllString(s, "_")
	if len(s) > 100 {
		s = s[:100]
	}
	return strings.TrimSpace(s)
}
