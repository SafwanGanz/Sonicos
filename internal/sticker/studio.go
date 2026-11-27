package sticker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/SafwanGanz/Sonicos/internal/command"
)

const (
	MaxVideoNoteBytes   = 25 << 20
	MaxVideoNoteSeconds = 60
	MaxAudioExtractSec  = 60
)

var (
	ErrStaticSilentSticker = errors.New("sticker is static and has no audio")

	meanVolRe = regexp.MustCompile(`mean_volume:\s*(-?[0-9]+(?:\.[0-9]+)?)\s*dB`)
	maxVolRe  = regexp.MustCompile(`max_volume:\s*(-?[0-9]+(?:\.[0-9]+)?)\s*dB`)
)

func (c *Converter) DubSoundSticker(ctx context.Context, visual, audio command.Attachment) (command.Sticker, error) {
	if err := validateAttachment(visual); err != nil {
		return command.Sticker{}, fmt.Errorf("visual: %w", err)
	}
	if err := validateAttachment(audio); err != nil {
		return command.Sticker{}, fmt.Errorf("audio: %w", err)
	}
	if visual.Kind != command.MediaImage && visual.Kind != command.MediaVideo && visual.Kind != command.MediaSticker {
		return command.Sticker{}, errors.New("visual input must be an image, video, or sticker")
	}
	if audio.Kind != command.MediaAudio && audio.Kind != command.MediaVideo {
		return command.Sticker{}, errors.New("audio input must be an audio file, voice note, or video")
	}

	audioPath, audioCleanup, err := writeInput(audio.Data, suffixFor(audio.MIMEType, audio.Kind))
	if err != nil {
		return command.Sticker{}, err
	}
	defer audioCleanup()

	audioInfo, err := c.probeMedia(ctx, audioPath)
	if err != nil {
		return command.Sticker{}, err
	}
	if !audioInfo.hasAudio {
		return command.Sticker{}, errors.New("audio input has no audio stream")
	}

	audioData, seconds, waveform, err := c.encodeOpusWithWaveform(ctx, audioPath, audioInfo.audioStreamIdx, MaxSoundStickerSeconds)
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

	if visual.Kind == command.MediaSticker {
		isAnimSticker := HasWebpAnimChunk(visual.Data) || CountWebpAnimFrames(visual.Data) > 1
		tryInject := func(buf []byte) bool {
			rebranded, off, injErr := InjectWebpExif(buf, meta, 512, 512)
			if injErr != nil {
				return false
			}
			anim := isAnimSticker || CountWebpAnimFrames(rebranded) > 1
			maxLimit := MaxStaticBytes
			if anim {
				maxLimit = MaxAnimatedBytes
			}
			if len(rebranded) <= maxLimit {
				webpData = rebranded
				exifOffset = off
				animated = anim
				return true
			}
			return false
		}
		if !tryInject(visual.Data) && isAnimSticker {
			if trimmed, ok := TrimAnimatedWebp(visual.Data, MaxAnimatedBytes-512); ok {
				tryInject(trimmed)
			}
		}

		if len(webpData) == 0 && isAnimSticker {
			return command.Sticker{}, errors.New("animated WebP sticker exceeds 500 KiB limit even after trimming frames")
		}
	}

	if len(webpData) == 0 {
		visPath, visCleanup, err := writeInput(visual.Data, suffixFor(visual.MIMEType, visual.Kind))
		if err != nil {
			return command.Sticker{}, err
		}
		defer visCleanup()

		visInfo, _ := c.probeMedia(ctx, visPath)
		animated = visual.Kind == command.MediaVideo ||
			strings.HasPrefix(strings.ToLower(visual.MIMEType), "image/gif") ||
			CountWebpAnimFrames(visual.Data) > 1
		clipMax := min(int(seconds), MaxSoundStickerSeconds)
		if clipMax < 1 {
			clipMax = MaxSoundStickerSeconds
		}
		webpData, exifOffset, err = c.encodeWebPWithLadder(ctx, visPath, visInfo, animated, clipMax, meta)
		if err != nil {
			return command.Sticker{}, err
		}
		if animated && CountWebpAnimFrames(webpData) <= 1 {
			animated = false
			webpData, exifOffset, err = c.encodeWebPWithLadder(ctx, visPath, visInfo, false, MaxSoundStickerSeconds, meta)
			if err != nil {
				return command.Sticker{}, err
			}
		}
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

func (c *Converter) RebrandSticker(ctx context.Context, webpData, audioData []byte, packName, author string, emojis []string) (command.Sticker, error) {
	if len(webpData) == 0 {
		return command.Sticker{}, errors.New("sticker data is empty")
	}
	packName = strings.TrimSpace(packName)
	if packName == "" {
		packName = "Sonicos"
	}
	author = strings.TrimSpace(author)
	if author == "" {
		author = "Sonicos"
	}
	if len(emojis) == 0 {
		if len(audioData) > 0 {
			emojis = []string{"🔊"}
		} else {
			emojis = []string{"✨"}
		}
	}

	meta := ExifMetadata{
		PackID:    newPackID("com.sonicos.steal."),
		PackName:  packName,
		Publisher: author,
		Emojis:    emojis,
	}
	rebranded, exifOffset, err := InjectWebpExif(webpData, meta, 512, 512)
	if err != nil {
		return command.Sticker{}, err
	}
	animated := CountWebpAnimFrames(rebranded) > 1

	st := command.Sticker{
		Data:       rebranded,
		MIMEType:   "image/webp",
		Animated:   animated,
		ExifOffset: exifOffset,
		Emojis:     emojis,
	}
	if len(audioData) > 0 {
		oggPath, cleanup, err := writeInput(audioData, ".ogg")
		if err != nil {
			return command.Sticker{}, err
		}
		defer cleanup()

		sec, durErr := c.duration(ctx, oggPath, MaxSoundStickerSeconds)
		if durErr != nil || sec == 0 {
			sec = 1
		}
		wave, waveErr := c.computeWaveform(ctx, oggPath)
		if waveErr != nil {
			wave = make([]byte, 64)
			for i := range wave {
				wave[i] = 52
			}
		}
		st.Audio = true
		st.AudioData = audioData
		st.AudioMIMEType = "audio/ogg; codecs=opus"
		st.Seconds = sec
		st.Waveform = wave
	}
	return st, nil
}

func (c *Converter) StickerToPNG(ctx context.Context, webpData []byte) ([]byte, error) {
	if len(webpData) == 0 {
		return nil, errors.New("sticker data is empty")
	}
	inPath, inCleanup, err := writeInput(webpData, ".webp")
	if err != nil {
		return nil, err
	}
	defer inCleanup()

	outPath, outCleanup, err := tempPath(".png")
	if err != nil {
		return nil, err
	}
	defer outCleanup()

	if err := c.run(ctx, "-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-i", inPath, "-frames:v", "1", "-f", "image2", outPath); err != nil {
		return nil, fmt.Errorf("convert sticker to PNG: %w", err)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		return nil, fmt.Errorf("read sticker PNG: %w", err)
	}
	return data, nil
}

func (c *Converter) StickerToMP4(ctx context.Context, webpData, audioData []byte) (command.VideoReply, error) {
	if len(webpData) == 0 {
		return command.VideoReply{}, errors.New("sticker data is empty")
	}

	isAnimated := CountWebpAnimFrames(webpData) > 1
	hasAudio := len(audioData) > 0
	if !isAnimated && !hasAudio {
		return command.VideoReply{}, ErrStaticSilentSticker
	}

	webpPath, webpCleanup, err := writeInput(webpData, ".webp")
	if err != nil {
		return command.VideoReply{}, err
	}
	defer webpCleanup()

	outPath, outCleanup, err := tempPath(".mp4")
	if err != nil {
		return command.VideoReply{}, err
	}
	defer outCleanup()

	const stickerVideoFilter = "format=rgba,premultiply=inplace=1,scale=512:512:force_original_aspect_ratio=decrease,pad=512:512:(ow-iw)/2:(oh-ih)/2:color=black,format=yuv420p"

	switch {
	case !isAnimated && hasAudio:
		audioPath, audioCleanup, err := writeInput(audioData, ".ogg")
		if err != nil {
			return command.VideoReply{}, err
		}
		defer audioCleanup()

		audioInfo, _ := c.probeMedia(ctx, audioPath)
		durArg := fmt.Sprint(MaxVideoNoteSeconds)
		if audioInfo.durationSec > 0 {
			dur := clampDuration(audioInfo.durationSec, 1, float64(MaxVideoNoteSeconds))
			durArg = fmt.Sprintf("%.2f", dur)
		}

		pngPath, pngCleanup, err := tempPath(".png")
		if err != nil {
			return command.VideoReply{}, err
		}
		defer pngCleanup()

		if err := c.run(ctx, "-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-i", webpPath, "-frames:v", "1", "-f", "image2", pngPath); err != nil {
			return command.VideoReply{}, fmt.Errorf("decode static sticker frame: %w", err)
		}
		if err := c.run(ctx,
			"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
			"-loop", "1", "-framerate", "15", "-t", durArg, "-i", pngPath,
			"-fflags", "+genpts", "-i", audioPath,
			"-map", "0:v:0", "-map", "1:a:0",
			"-t", durArg,
			"-vf", stickerVideoFilter,
			"-af", "asetpts=PTS-STARTPTS,aresample=44100:async=1:first_pts=0",
			"-c:v", "libx264", "-profile:v", "baseline", "-level", "3.1", "-pix_fmt", "yuv420p",
			"-preset", "veryfast", "-crf", "23", "-tune", "stillimage",
			"-c:a", "aac", "-b:a", "128k", "-ar", "44100",
			"-movflags", "+faststart", "-shortest", outPath,
		); err != nil {
			return command.VideoReply{}, fmt.Errorf("encode static sound sticker to mp4: %w", err)
		}

	case isAnimated && !hasAudio:
		if err := c.run(ctx,
			"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
			"-i", webpPath,
			"-t", fmt.Sprint(MaxVideoNoteSeconds),
			"-vf", stickerVideoFilter,
			"-c:v", "libx264", "-profile:v", "baseline", "-level", "3.1", "-pix_fmt", "yuv420p",
			"-preset", "veryfast", "-crf", "23", "-an",
			"-movflags", "+faststart", outPath,
		); err != nil {
			return command.VideoReply{}, fmt.Errorf("encode animated sticker to mp4: %w", err)
		}

	default:
		midPath, midCleanup, err := tempPath(".mp4")
		if err != nil {
			return command.VideoReply{}, err
		}
		defer midCleanup()

		if err := c.run(ctx,
			"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
			"-i", webpPath,
			"-t", fmt.Sprint(MaxVideoNoteSeconds),
			"-vf", stickerVideoFilter,
			"-c:v", "libx264", "-profile:v", "baseline", "-level", "3.1", "-pix_fmt", "yuv420p",
			"-preset", "veryfast", "-crf", "23", "-an",
			"-movflags", "+faststart", midPath,
		); err != nil {
			return command.VideoReply{}, fmt.Errorf("decode animated sticker to mp4: %w", err)
		}

		audioPath, audioCleanup, err := writeInput(audioData, ".ogg")
		if err != nil {
			return command.VideoReply{}, err
		}
		defer audioCleanup()

		midInfo, _ := c.probeMedia(ctx, midPath)
		audioInfo, _ := c.probeMedia(ctx, audioPath)

		targetArg := fmt.Sprint(MaxVideoNoteSeconds)
		needLoop := true
		if audioInfo.durationSec > 0 {
			targetDur := max(audioInfo.durationSec, midInfo.durationSec)
			targetDur = clampDuration(targetDur, 1, float64(MaxVideoNoteSeconds))
			targetArg = fmt.Sprintf("%.2f", targetDur)
			needLoop = midInfo.durationSec <= 0 || midInfo.durationSec+0.1 < targetDur
		}

		args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y"}
		if needLoop {
			args = append(args, "-stream_loop", "-1")
		}
		args = append(args,
			"-i", midPath,
			"-fflags", "+genpts", "-i", audioPath,
			"-map", "0:v:0", "-map", "1:a:0",
			"-t", targetArg,
			"-c:v", "copy",
			"-af", "asetpts=PTS-STARTPTS,aresample=44100:async=1:first_pts=0",
			"-c:a", "aac", "-b:a", "128k", "-ar", "44100",
			"-movflags", "+faststart", "-shortest", outPath,
		)
		if err := c.run(ctx, args...); err != nil {
			return command.VideoReply{}, fmt.Errorf("mux animated sound sticker to mp4: %w", err)
		}
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		return command.VideoReply{}, fmt.Errorf("read converted mp4: %w", err)
	}
	if len(data) > MaxVideoNoteBytes {
		return command.VideoReply{}, fmt.Errorf("converted video exceeds %d MiB limit", MaxVideoNoteBytes>>20)
	}

	sec, err := c.duration(ctx, outPath, MaxVideoNoteSeconds)
	if err != nil || sec == 0 {
		sec = 1
	}

	thumbPath, thumbCleanup, err := tempPath(".jpg")
	if err != nil {
		return command.VideoReply{}, err
	}
	defer thumbCleanup()
	var thumb []byte
	if err := c.run(ctx, "-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-i", outPath, "-frames:v", "1", "-vf", "scale=320:-2", "-q:v", "6", thumbPath); err == nil {
		thumb, _ = os.ReadFile(thumbPath)
	}

	return command.VideoReply{
		Data:    data,
		Thumb:   thumb,
		Seconds: sec,
		Width:   512,
		Height:  512,
	}, nil
}

func clampDuration(v, minSec, maxSec float64) float64 {
	if v < minSec {
		return minSec
	}
	if v > maxSec {
		return maxSec
	}
	return v
}

func (c *Converter) ExtractAudio(ctx context.Context, attachment command.Attachment) (command.AudioReply, error) {
	if err := validateAttachment(attachment); err != nil {
		return command.AudioReply{}, err
	}
	if attachment.Kind != command.MediaAudio && attachment.Kind != command.MediaVideo {
		return command.AudioReply{}, errors.New("audio extraction requires audio or a video with sound")
	}
	inPath, cleanup, err := writeInput(attachment.Data, suffixFor(attachment.MIMEType, attachment.Kind))
	if err != nil {
		return command.AudioReply{}, err
	}
	defer cleanup()

	info, err := c.probeMedia(ctx, inPath)
	if err != nil {
		return command.AudioReply{}, err
	}
	if !info.hasAudio {
		return command.AudioReply{}, errors.New("input media has no audio stream")
	}

	data, sec, wave, err := c.encodeOpusWithWaveform(ctx, inPath, info.audioStreamIdx, MaxAudioExtractSec)
	if err != nil {
		return command.AudioReply{}, err
	}
	return command.AudioReply{
		Data:     data,
		MIMEType: "audio/ogg; codecs=opus",
		Seconds:  sec,
		Waveform: wave,
		PTT:      false,
	}, nil
}

func (c *Converter) ToVideoNote(ctx context.Context, attachment command.Attachment) (command.VideoNoteReply, error) {
	if err := validateAttachment(attachment); err != nil {
		return command.VideoNoteReply{}, err
	}
	if attachment.Kind != command.MediaVideo && attachment.Kind != command.MediaImage {
		return command.VideoNoteReply{}, errors.New("video note requires a video or image")
	}

	inPath, inCleanup, err := writeInput(attachment.Data, suffixFor(attachment.MIMEType, attachment.Kind))
	if err != nil {
		return command.VideoNoteReply{}, err
	}
	defer inCleanup()

	info, err := c.probeMedia(ctx, inPath)
	if err != nil {
		return command.VideoNoteReply{}, err
	}
	if !info.hasVideo && !info.hasAttachedPic && attachment.Kind != command.MediaImage {
		return command.VideoNoteReply{}, errors.New("input has no video or image stream")
	}

	outPath, outCleanup, err := tempPath(".mp4")
	if err != nil {
		return command.VideoNoteReply{}, err
	}
	defer outCleanup()

	const cropScale = "crop='min(iw,ih)':'min(iw,ih)',scale=384:384,fps=30,format=yuv420p"
	var args []string

	switch {
	case attachment.Kind == command.MediaImage || !info.hasVideo:
		const imageDur = "5"
		args = []string{
			"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
			"-loop", "1", "-t", imageDur, "-i", inPath,
			"-f", "lavfi", "-t", imageDur, "-i", "anullsrc=channel_layout=mono:sample_rate=44100",
			"-vf", cropScale,
			"-map", "0:v:0", "-map", "1:a:0",
		}
	case info.hasAudio:
		audioMap := "0:a:0"
		if info.audioStreamIdx >= 0 {
			audioMap = fmt.Sprintf("0:%d", info.audioStreamIdx)
		}
		args = []string{
			"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
			"-fflags", "+genpts", "-i", inPath,
			"-t", fmt.Sprint(MaxVideoNoteSeconds),
			"-vf", "setpts=PTS-STARTPTS," + cropScale,
			"-af", "asetpts=PTS-STARTPTS,aresample=44100:async=1:first_pts=0",
			"-map", "0:v:0", "-map", audioMap,
		}
	default:
		durCap := float64(MaxVideoNoteSeconds)
		if info.durationSec > 0 && info.durationSec < durCap {
			durCap = info.durationSec
		}
		if durCap < 1 {
			durCap = 5
		}
		args = []string{
			"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
			"-fflags", "+genpts", "-i", inPath,
			"-f", "lavfi", "-t", fmt.Sprintf("%.2f", durCap), "-i", "anullsrc=channel_layout=mono:sample_rate=44100",
			"-t", fmt.Sprint(MaxVideoNoteSeconds),
			"-vf", "setpts=PTS-STARTPTS," + cropScale,
			"-map", "0:v:0", "-map", "1:a:0",
		}
	}

	args = append(args,
		"-c:v", "libx264", "-profile:v", "baseline", "-level", "3.1", "-pix_fmt", "yuv420p",
		"-preset", "veryfast", "-crf", "24",
		"-c:a", "aac", "-b:a", "64k", "-ar", "44100", "-ac", "1",
		"-movflags", "+faststart", "-shortest", outPath,
	)

	if err := c.run(ctx, args...); err != nil {
		return command.VideoNoteReply{}, fmt.Errorf("encode video note: %w", err)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		return command.VideoNoteReply{}, fmt.Errorf("read video note: %w", err)
	}
	if len(data) > MaxVideoNoteBytes {
		return command.VideoNoteReply{}, fmt.Errorf("video note exceeds %d MiB limit", MaxVideoNoteBytes>>20)
	}

	sec, err := c.duration(ctx, outPath, MaxVideoNoteSeconds)
	if err != nil || sec == 0 {
		sec = 1
	}

	thumbPath, thumbCleanup, err := tempPath(".jpg")
	if err != nil {
		return command.VideoNoteReply{}, err
	}
	defer thumbCleanup()
	var thumb []byte
	if err := c.run(ctx, "-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-i", outPath, "-frames:v", "1", "-vf", "scale=320:-2", "-q:v", "6", thumbPath); err == nil {
		thumb, _ = os.ReadFile(thumbPath)
	}

	return command.VideoNoteReply{
		Data:    data,
		Thumb:   thumb,
		Seconds: sec,
		Width:   384,
		Height:  384,
	}, nil
}

func (c *Converter) MeasureLoudness(ctx context.Context, audioData []byte) (meanDB float64, maxDB float64, err error) {
	if len(audioData) == 0 {
		return -91, -91, errors.New("audio data is empty")
	}
	inPath, cleanup, err := writeInput(audioData, ".ogg")
	if err != nil {
		return 0, 0, err
	}
	defer cleanup()

	cmd := exec.CommandContext(ctx, c.ffmpeg, "-hide_banner", "-nostdin", "-i", inPath, "-vn", "-af", "volumedetect", "-f", "null", "-")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return 0, 0, fmt.Errorf("measure loudness: %w: %s", err, msg)
		}
		return 0, 0, fmt.Errorf("measure loudness: %w", err)
	}
	logOut := stderr.String()
	mMean := meanVolRe.FindStringSubmatch(logOut)
	mMax := maxVolRe.FindStringSubmatch(logOut)
	if len(mMean) < 2 || len(mMax) < 2 {
		return -91, -91, errors.New("volumedetect output missing volume metrics")
	}
	meanVal, err1 := strconv.ParseFloat(mMean[1], 64)
	maxVal, err2 := strconv.ParseFloat(mMax[1], 64)
	if err1 != nil || err2 != nil || math.IsNaN(meanVal) || math.IsNaN(maxVal) {
		return -91, -91, errors.New("invalid volumedetect numbers")
	}
	return meanVal, maxVal, nil
}
