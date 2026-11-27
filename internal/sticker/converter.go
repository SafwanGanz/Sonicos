package sticker

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/SafwanGanz/Sonicos/internal/command"
)

const (
	MaxInputBytes          = 20 << 20
	MaxStaticBytes         = 100 << 10
	MaxAnimatedBytes       = 500 << 10
	MaxAudioBytes          = 4 << 20
	MaxStickerSeconds      = 10
	MaxSoundStickerSeconds = 25
	MaxAudioSeconds        = 25

	targetStaticBytes   = 98 << 10
	targetAnimatedBytes = 490 << 10
)

var alphaPixFmtRe = regexp.MustCompile(`(?i)(yuva|rgba|bgra|argb|abgr|pal8|ya)`)

type Converter struct {
	ffmpeg string
}

type mediaProbe struct {
	hasVideo       bool
	hasAttachedPic bool
	hasAudio       bool
	audioStreamIdx int
	width          int
	height         int
	pixFmt         string
	durationSec    float64
}

func NewConverter(ffmpeg string) *Converter {
	if strings.TrimSpace(ffmpeg) == "" {
		ffmpeg = "ffmpeg"
	}
	return &Converter{ffmpeg: ffmpeg}
}

func (c *Converter) ToWebP(ctx context.Context, attachment command.Attachment) (command.Sticker, error) {
	if err := validateAttachment(attachment); err != nil {
		return command.Sticker{}, err
	}
	if attachment.Kind != command.MediaImage && attachment.Kind != command.MediaVideo {
		return command.Sticker{}, errors.New("sticker command requires an image or video")
	}

	inputPath, cleanup, err := writeInput(attachment.Data, suffixFor(attachment.MIMEType, attachment.Kind))
	if err != nil {
		return command.Sticker{}, err
	}
	defer cleanup()

	info, _ := c.probeMedia(ctx, inputPath)
	animated := attachment.Kind == command.MediaVideo || strings.HasPrefix(strings.ToLower(attachment.MIMEType), "image/gif")
	meta := ExifMetadata{
		PackID:    newPackID("com.sonicos.pack."),
		PackName:  "Sonicos Stickers",
		Publisher: "Sonicos",
		Emojis:    []string{"✨"},
	}

	webpData, exifOffset, err := c.encodeWebPWithLadder(ctx, inputPath, info, animated, MaxStickerSeconds, meta)
	if err != nil {
		return command.Sticker{}, err
	}
	if animated && CountWebpAnimFrames(webpData) <= 1 {
		animated = false
		webpData, exifOffset, err = c.encodeWebPWithLadder(ctx, inputPath, info, false, MaxStickerSeconds, meta)
		if err != nil {
			return command.Sticker{}, err
		}
	}
	return command.Sticker{
		Data:       webpData,
		MIMEType:   "image/webp",
		Animated:   animated,
		ExifOffset: exifOffset,
		Emojis:     meta.Emojis,
	}, nil
}

func (c *Converter) ToSoundSticker(ctx context.Context, attachment command.Attachment) (command.Sticker, error) {
	if err := validateAttachment(attachment); err != nil {
		return command.Sticker{}, err
	}
	if attachment.Kind != command.MediaAudio && attachment.Kind != command.MediaVideo {
		return command.Sticker{}, errors.New("audio sticker command requires audio or video with an audio track")
	}

	inputPath, cleanup, err := writeInput(attachment.Data, suffixFor(attachment.MIMEType, attachment.Kind))
	if err != nil {
		return command.Sticker{}, err
	}
	defer cleanup()

	info, err := c.probeMedia(ctx, inputPath)
	if err != nil {
		return command.Sticker{}, err
	}
	if !info.hasAudio {
		return command.Sticker{}, errors.New("input media has no audio stream")
	}

	audioData, seconds, waveform, err := c.encodeOpusWithWaveform(ctx, inputPath, info.audioStreamIdx, MaxSoundStickerSeconds)
	if err != nil {
		return command.Sticker{}, err
	}

	meta := ExifMetadata{
		PackID:    newPackID("com.sonicos.sound."),
		PackName:  "Sonicos Sound",
		Publisher: "Sonicos",
		Emojis:    []string{"🔊"},
	}

	var webpData []byte
	var exifOffset uint32
	animated := false

	switch {
	case attachment.Kind == command.MediaVideo && info.hasVideo:
		animated = true
		clipMax := min(int(seconds), MaxSoundStickerSeconds)
		if clipMax < 1 {
			clipMax = MaxSoundStickerSeconds
		}
		webpData, exifOffset, err = c.encodeWebPWithLadder(ctx, inputPath, info, true, clipMax, meta)
		if err == nil && CountWebpAnimFrames(webpData) <= 1 {
			animated = false
			webpData, exifOffset, err = c.encodeWebPWithLadder(ctx, inputPath, info, false, MaxSoundStickerSeconds, meta)
		}
	case info.hasAttachedPic:
		webpData, exifOffset, err = c.encodeWebPWithLadder(ctx, inputPath, info, false, MaxSoundStickerSeconds, meta)
	default:
		coverPNG, coverErr := renderAudioCoverPNG(waveform)
		if coverErr != nil {
			return command.Sticker{}, fmt.Errorf("render audio sticker cover: %w", coverErr)
		}
		coverPath, coverCleanup, writeErr := writeInput(coverPNG, ".png")
		if writeErr != nil {
			return command.Sticker{}, writeErr
		}
		coverInfo := mediaProbe{hasVideo: true, width: 512, height: 512, pixFmt: "rgba"}
		webpData, exifOffset, err = c.encodeWebPWithLadder(ctx, coverPath, coverInfo, false, MaxSoundStickerSeconds, meta)
		coverCleanup()
	}
	if err != nil {
		return command.Sticker{}, err
	}

	return command.Sticker{
		Data:          webpData,
		MIMEType:      "image/webp",
		Animated:      animated,
		ExifOffset:    exifOffset,
		Emojis:        meta.Emojis,
		Audio:         true,
		AudioData:     audioData,
		AudioMIMEType: "audio/ogg; codecs=opus",
		Seconds:       seconds,
		Waveform:      waveform,
	}, nil
}

func (c *Converter) ToOpus(ctx context.Context, attachment command.Attachment) (command.Sticker, error) {
	if err := validateAttachment(attachment); err != nil {
		return command.Sticker{}, err
	}
	if attachment.Kind != command.MediaAudio && attachment.Kind != command.MediaVideo {
		return command.Sticker{}, errors.New("audio sticker command requires audio or video with an audio track")
	}

	inputPath, cleanup, err := writeInput(attachment.Data, suffixFor(attachment.MIMEType, attachment.Kind))
	if err != nil {
		return command.Sticker{}, err
	}
	defer cleanup()

	info, _ := c.probeMedia(ctx, inputPath)
	data, seconds, waveform, err := c.encodeOpusWithWaveform(ctx, inputPath, info.audioStreamIdx, MaxAudioSeconds)
	if err != nil {
		return command.Sticker{}, err
	}
	return command.Sticker{
		Data:          data,
		MIMEType:      "audio/ogg; codecs=opus",
		Audio:         true,
		AudioMIMEType: "audio/ogg; codecs=opus",
		Seconds:       seconds,
		Waveform:      waveform,
	}, nil
}

func validateAttachment(attachment command.Attachment) error {
	if len(attachment.Data) == 0 {
		return errors.New("media attachment is empty")
	}
	if len(attachment.Data) > MaxInputBytes {
		return fmt.Errorf("input exceeds %d MiB", MaxInputBytes>>20)
	}
	return nil
}

func (c *Converter) encodeOpusWithWaveform(ctx context.Context, inputPath string, audioStreamIdx, maxSeconds int) ([]byte, uint32, []byte, error) {
	outputPath, removeOutput, err := tempPath(".ogg")
	if err != nil {
		return nil, 0, nil, err
	}
	defer removeOutput()

	mapSpec := "0:a:0"
	if audioStreamIdx >= 0 {
		mapSpec = fmt.Sprintf("0:%d", audioStreamIdx)
	}
	args := []string{
		"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-fflags", "+genpts",
		"-i", inputPath,
		"-map", mapSpec, "-map_metadata", "-1", "-map_chapters", "-1", "-vn",
		"-af", "asetpts=PTS-STARTPTS,aresample=48000:async=1:first_pts=0",
		"-t", fmt.Sprint(maxSeconds),
		"-ac", "2", "-ar", "48000", "-c:a", "libopus", "-b:a", "48k",
		"-vbr", "on", "-compression_level", "10", "-frame_duration", "20",
		"-application", "audio", "-f", "ogg", outputPath,
	}
	if err := c.run(ctx, args...); err != nil {
		return nil, 0, nil, fmt.Errorf("convert audio to Opus: %w", err)
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("read converted Opus audio: %w", err)
	}
	if len(data) > MaxAudioBytes {
		return nil, 0, nil, fmt.Errorf("converted Opus audio exceeds %d KiB", MaxAudioBytes>>10)
	}
	seconds, err := c.duration(ctx, outputPath, maxSeconds)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("read Opus duration: %w", err)
	}
	waveform, err := c.computeWaveform(ctx, outputPath)
	if err != nil {

		waveform = make([]byte, 64)
		for i := range waveform {
			waveform[i] = 52
		}
	}
	return data, seconds, waveform, nil
}

func (c *Converter) encodeWebPWithLadder(
	ctx context.Context,
	inputPath string,
	info mediaProbe,
	animated bool,
	maxSeconds int,
	meta ExifMetadata,
) ([]byte, uint32, error) {
	opaqueSquare := info.width > 0 && info.width == info.height && !alphaPixFmtRe.MatchString(info.pixFmt)
	scaleFilter := "scale=512:512:force_original_aspect_ratio=decrease:flags=lanczos,setsar=1,pad=512:512:(ow-iw)/2:(oh-ih)/2:color=0x00000000,format=yuva420p"
	if opaqueSquare {
		scaleFilter = "scale=512:512:flags=lanczos,setsar=1,format=yuv420p"
	}

	type attempt struct {
		quality int
		fps     int
	}
	var attempts []attempt
	targetLimit := targetStaticBytes
	hardLimit := MaxStaticBytes
	if !animated {
		attempts = []attempt{{85, 0}, {75, 0}, {65, 0}, {50, 0}, {35, 0}}
	} else {
		targetLimit = targetAnimatedBytes
		hardLimit = MaxAnimatedBytes
		if info.durationSec > 6 {
			attempts = []attempt{{52, 12}, {40, 10}, {28, 8}, {20, 6}}
		} else {
			attempts = []attempt{{60, 15}, {48, 12}, {36, 10}, {26, 8}, {20, 6}}
		}
	}

	var best []byte
	var bestOffset uint32
	for _, att := range attempts {
		outputPath, removeOutput, err := tempPath(".webp")
		if err != nil {
			return nil, 0, err
		}
		err = c.encodeWebP(ctx, inputPath, outputPath, scaleFilter, animated, maxSeconds, att.quality, att.fps)
		if err != nil {
			removeOutput()
			return nil, 0, err
		}
		raw, readErr := os.ReadFile(outputPath)
		removeOutput()
		if readErr != nil {
			return nil, 0, fmt.Errorf("read converted WebP: %w", readErr)
		}
		withExif, exifOffset, exifErr := InjectWebpExif(raw, meta, 512, 512)
		if exifErr != nil {
			return nil, 0, exifErr
		}
		if best == nil || len(withExif) < len(best) {
			best = withExif
			bestOffset = exifOffset
		}
		if len(withExif) <= targetLimit {
			return withExif, exifOffset, nil
		}
	}

	if len(best) > 0 && len(best) <= hardLimit {
		return best, bestOffset, nil
	}
	return nil, 0, fmt.Errorf("converted sticker exceeds WhatsApp's %d KiB size limit", hardLimit>>10)
}

func (c *Converter) encodeWebP(
	ctx context.Context,
	inputPath, outputPath, scaleFilter string,
	animated bool,
	maxSeconds, quality, fps int,
) error {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-i", inputPath, "-map", "0:v:0"}
	if animated {
		args = append(args,
			"-t", fmt.Sprint(maxSeconds),
			"-an",
			"-vf", fmt.Sprintf("setpts=PTS-STARTPTS,fps=%d,%s", fps, scaleFilter),
			"-vcodec", "libwebp_anim",
			"-lossless", "0",
			"-q:v", fmt.Sprint(quality),
			"-compression_level", "4",
			"-loop", "0",
			"-preset", "default",
		)
	} else {
		args = append(args,
			"-frames:v", "1",
			"-an",
			"-vf", scaleFilter,
			"-vcodec", "libwebp",
			"-lossless", "0",
			"-q:v", fmt.Sprint(quality),
			"-compression_level", "6",
		)
	}
	args = append(args, outputPath)
	return c.run(ctx, args...)
}

func (c *Converter) computeWaveform(ctx context.Context, oggPath string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.ffmpeg, "-v", "error", "-nostdin", "-i", oggPath, "-ac", "1", "-ar", "8000", "-f", "s16le", "pipe:1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	pcm, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return nil, fmt.Errorf("%w: %s", err, msg)
		}
		return nil, err
	}

	out := make([]byte, 64)
	totalSamples := len(pcm) / 2
	if totalSamples < 64 {
		for i := range out {
			out[i] = 18
		}
		return out, nil
	}

	blockSize := totalSamples / 64
	peaks := make([]float64, 64)
	var maxVal float64
	for b := range 64 {
		start := b * blockSize
		end := start + blockSize
		if b == 63 {
			end = totalSamples
		}
		var sumSq float64
		for s := start; s < end; s++ {
			sample := float64(int16(binary.LittleEndian.Uint16(pcm[s*2:s*2+2]))) / 32768.0
			sumSq += sample * sample
		}
		rms := math.Sqrt(sumSq / float64(max(1, end-start)))
		peaks[b] = rms
		if rms > maxVal {
			maxVal = rms
		}
	}

	scale := 1.0
	if maxVal > 1e-4 {
		scale = 1.0 / maxVal
	}
	for i := range 64 {
		scaled := math.Pow(peaks[i]*scale, 0.72) * 100.0
		v := math.Round(scaled)
		if v < 0 {
			v = 0
		} else if v > 100 {
			v = 100
		}
		out[i] = byte(v)
	}
	return out, nil
}

func (c *Converter) probeMedia(ctx context.Context, path string) (mediaProbe, error) {
	cmd := exec.CommandContext(ctx, c.ffprobePath(), "-v", "error", "-show_format", "-show_streams", "-of", "json", path)
	output, err := cmd.Output()
	if err != nil {
		return mediaProbe{}, fmt.Errorf("run ffprobe: %w", err)
	}
	var parsed struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
		Streams []struct {
			Index       int    `json:"index"`
			CodecType   string `json:"codec_type"`
			Width       int    `json:"width"`
			Height      int    `json:"height"`
			Channels    int    `json:"channels"`
			PixFmt      string `json:"pix_fmt"`
			Duration    string `json:"duration"`
			Disposition struct {
				Default     int `json:"default"`
				AttachedPic int `json:"attached_pic"`
			} `json:"disposition"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(output, &parsed); err != nil {
		return mediaProbe{}, fmt.Errorf("parse ffprobe output: %w", err)
	}

	res := mediaProbe{audioStreamIdx: -1}
	if d, err := strconv.ParseFloat(strings.TrimSpace(parsed.Format.Duration), 64); err == nil && d > 0 {
		res.durationSec = d
	}
	audioScore := -1
	for _, s := range parsed.Streams {
		switch s.CodecType {
		case "video":
			if s.Disposition.AttachedPic != 0 {
				res.hasAttachedPic = true
				if res.width == 0 {
					res.width = s.Width
					res.height = s.Height
					res.pixFmt = s.PixFmt
				}
				continue
			}
			if !res.hasVideo {
				res.hasVideo = true
				res.width = s.Width
				res.height = s.Height
				res.pixFmt = s.PixFmt
				if res.durationSec == 0 {
					if d, err := strconv.ParseFloat(strings.TrimSpace(s.Duration), 64); err == nil && d > 0 {
						res.durationSec = d
					}
				}
			}
		case "audio":
			res.hasAudio = true
			score := 0
			if s.Channels > 0 {
				score += 1
			}
			if s.Disposition.Default != 0 {
				score += 2
			}
			if score > audioScore {
				audioScore = score
				res.audioStreamIdx = s.Index
			}
			if res.durationSec == 0 {
				if d, err := strconv.ParseFloat(strings.TrimSpace(s.Duration), 64); err == nil && d > 0 {
					res.durationSec = d
				}
			}
		}
	}
	return res, nil
}

func (c *Converter) run(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, c.ffmpeg, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message != "" {
			return fmt.Errorf("%w: %s", err, message)
		}
		return err
	}
	return nil
}

func (c *Converter) duration(ctx context.Context, path string, maxSeconds int) (uint32, error) {
	cmd := exec.CommandContext(ctx, c.ffprobePath(), "-v", "error", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", path)
	output, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("run ffprobe: %w", err)
	}
	seconds, err := strconv.ParseFloat(strings.TrimSpace(string(output)), 64)
	if err != nil {
		return 0, fmt.Errorf("parse duration %q: %w", strings.TrimSpace(string(output)), err)
	}
	if seconds <= 0 {
		return 0, fmt.Errorf("converted audio duration %.2fs is outside the supported range", seconds)
	}
	if seconds > float64(maxSeconds) {

		if seconds <= float64(maxSeconds)+0.5 {
			return uint32(maxSeconds), nil
		}
		return 0, fmt.Errorf("converted audio duration %.2fs is outside the supported range", seconds)
	}
	rounded := uint32(math.Round(seconds))
	if rounded < 1 {
		rounded = 1
	}
	return rounded, nil
}

func (c *Converter) ffprobePath() string {
	base := filepath.Base(c.ffmpeg)
	probeName := "ffprobe"
	if strings.HasSuffix(strings.ToLower(base), ".exe") {
		probeName = "ffprobe.exe"
	}
	if !strings.ContainsRune(c.ffmpeg, os.PathSeparator) && base == c.ffmpeg {
		return probeName
	}
	if strings.EqualFold(strings.TrimSuffix(base, filepath.Ext(base)), "ffmpeg") {
		dir := filepath.Dir(c.ffmpeg)
		if dir == "." {
			return "." + string(os.PathSeparator) + probeName
		}
		return filepath.Join(dir, probeName)
	}
	return probeName
}

func writeInput(data []byte, suffix string) (string, func(), error) {
	path, cleanup, err := tempPath(suffix)
	if err != nil {
		return "", nil, err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("write media input: %w", err)
	}
	return path, cleanup, nil
}

func tempPath(suffix string) (string, func(), error) {
	file, err := os.CreateTemp("", "sonicos-media-*")
	if err != nil {
		return "", nil, fmt.Errorf("create media temp file: %w", err)
	}
	path := file.Name() + suffix
	if err := file.Close(); err != nil {
		_ = os.Remove(file.Name())
		return "", nil, fmt.Errorf("close media temp file: %w", err)
	}
	if err := os.Rename(file.Name(), path); err != nil {
		_ = os.Remove(file.Name())
		return "", nil, fmt.Errorf("prepare media temp file: %w", err)
	}
	return path, func() { _ = os.Remove(path) }, nil
}

func suffixFor(mimeType string, kind command.MediaKind) string {
	if strings.Contains(mimeType, "/") {
		subtype := strings.ToLower(strings.TrimSpace(strings.SplitN(strings.SplitN(mimeType, ";", 2)[0], "/", 2)[1]))
		switch subtype {
		case "jpeg":
			return ".jpg"
		case "quicktime":
			return ".mov"
		case "x-matroska":
			return ".mkv"
		}
		if clean := sanitizeExt(subtype); clean != "" {
			return "." + clean
		}
	}
	switch kind {
	case command.MediaImage:
		return ".img"
	case command.MediaVideo:
		return ".video"
	case command.MediaAudio:
		return ".audio"
	default:
		return ".bin"
	}
}

func sanitizeExt(subtype string) string {
	if subtype == "" || strings.Contains(subtype, "..") {
		return ""
	}
	var b strings.Builder
	for _, r := range subtype {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '+' || r == '-' {
			b.WriteRune(r)
		} else {
			return ""
		}
	}
	return strings.Trim(b.String(), ".")
}
