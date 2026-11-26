package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/SafwanGanz/Sonicos/internal/command"
)

const instagramAPI = "https://apiii-xrina.my.id/download/instagram"

type instagramResponse struct {
	Status  bool   `json:"status"`
	Creator string `json:"creator"`
	Data    struct {
		SourceURL string `json:"sourceUrl"`
		Type      string `json:"type"`
		Videos    []struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"videos"`
		Images []struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"images"`
		Media []struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"media"`
	} `json:"data"`
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

func init() {
	command.Register(command.Command{
		Name:        "ig",
		Aliases:     []string{"insta", "instagram"},
		Category:    "Media",
		Description: "Download Instagram reel/post media. Usage: !ig <url>",
		Handler:     instagramDownloader,
	})
}

func instagramDownloader(ctx context.Context, message command.Message, args []string, reply command.ReplyFunc) error {
	if len(args) == 0 {
		return reply(ctx, command.Reply{Text: "Usage: !ig <instagram_url>"})
	}

	rawURL := strings.TrimSpace(args[0])
	if !strings.Contains(rawURL, "instagram.com") {
		return reply(ctx, command.Reply{Text: "Please provide a valid Instagram URL"})
	}

	apiURL := fmt.Sprintf("%s?url=%s", instagramAPI, url.QueryEscape(rawURL))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		slog.Error("create instagram api request", "error", err)
		return reply(ctx, command.Reply{Text: "Failed to create request"})
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		slog.Error("call instagram api", "error", err)
		return reply(ctx, command.Reply{Text: "Failed to fetch from Instagram API"})
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		slog.Error("read instagram api response", "error", err)
		return reply(ctx, command.Reply{Text: "Failed to read API response"})
	}

	var result instagramResponse
	if err := json.Unmarshal(body, &result); err != nil {
		slog.Error("parse instagram api response", "error", err)
		return reply(ctx, command.Reply{Text: "Failed to parse API response"})
	}

	if !result.Status || result.Data.Type == "" {
		return reply(ctx, command.Reply{Text: "Could not extract media from the provided URL"})
	}

	var mediaURL string
	var mediaType string

	switch result.Data.Type {
	case "video":
		if len(result.Data.Videos) > 0 {
			mediaURL = result.Data.Videos[0].URL
			mediaType = "video"
		} else if len(result.Data.Media) > 0 {
			mediaURL = result.Data.Media[0].URL
			mediaType = "video"
		}
	case "image":
		if len(result.Data.Images) > 0 {
			mediaURL = result.Data.Images[0].URL
			mediaType = "image"
		} else if len(result.Data.Media) > 0 {
			mediaURL = result.Data.Media[0].URL
			mediaType = "image"
		}
	}

	if mediaURL == "" {
		return reply(ctx, command.Reply{Text: "No media found in the response"})
	}

	mediaData, mimeType, err := downloadMedia(ctx, mediaURL)
	if err != nil {
		slog.Error("download media", "error", err, "url", mediaURL)
		return reply(ctx, command.Reply{Text: "Failed to download media"})
	}

	switch mediaType {
	case "video":
		return reply(ctx, command.Reply{
			Video: &command.VideoReply{
				Data:    mediaData,
				Caption: "Downloaded via Sonicos",
			},
		})
	case "image":
		return reply(ctx, command.Reply{
			Image: &command.ImageReply{
				Data:     mediaData,
				MIMEType: mimeType,
				Caption:  "Downloaded via Sonicos",
			},
		})
	default:
		return reply(ctx, command.Reply{Text: "Unknown media type"})
	}
}

func downloadMedia(ctx context.Context, mediaURL string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, mediaURL, nil)
	if err != nil {
		return nil, "", err
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Sonicos/1.0)")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("download failed with status: %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}

	mimeType := resp.Header.Get("Content-Type")
	if mimeType == "" {
		if strings.HasSuffix(mediaURL, ".mp4") || strings.Contains(mediaURL, "video") {
			mimeType = "video/mp4"
		} else {
			mimeType = "image/jpeg"
		}
	}

	return data, mimeType, nil
}