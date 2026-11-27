package sticker

import (
	"bytes"
	"context"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/SafwanGanz/Sonicos/internal/command"
)

func makeWebpChunk(fourcc string, payload []byte) []byte {
	padded := len(payload) + (len(payload) & 1)
	buf := make([]byte, 8+padded)
	copy(buf[0:4], fourcc)
	binary.LittleEndian.PutUint32(buf[4:8], uint32(len(payload)))
	copy(buf[8:], payload)
	return buf
}

func makeTestWebP(chunks ...[]byte) []byte {
	bodyLen := 4
	for _, c := range chunks {
		bodyLen += len(c)
	}
	out := make([]byte, 12, 8+bodyLen)
	copy(out[0:4], "RIFF")
	binary.LittleEndian.PutUint32(out[4:8], uint32(bodyLen))
	copy(out[8:12], "WEBP")
	for _, c := range chunks {
		out = append(out, c...)
	}
	return out
}

func TestHasWebpAnimChunkAndTrimAnimatedWebp(t *testing.T) {
	staticWebp := makeTestWebP(makeWebpChunk("VP8L", []byte{0x2f, 0x00, 0x00, 0x00, 0x00, 0x01, 0x02}))
	if HasWebpAnimChunk(staticWebp) {
		t.Fatal("expected static WebP not to report HasWebpAnimChunk")
	}

	vp8x := make([]byte, 10)
	vp8x[0] = 0x02
	anim := make([]byte, 6)
	framePayload := bytes.Repeat([]byte{0xAA}, 120<<10)

	oneFrameAnim := makeTestWebP(
		makeWebpChunk("VP8X", vp8x),
		makeWebpChunk("ANIM", anim),
		makeWebpChunk("ANMF", framePayload[:1024]),
	)
	if !HasWebpAnimChunk(oneFrameAnim) {
		t.Fatal("expected 1-frame ANIM/ANMF WebP to report HasWebpAnimChunk")
	}

	oversizedAnim := makeTestWebP(
		makeWebpChunk("VP8X", vp8x),
		makeWebpChunk("ANIM", anim),
		makeWebpChunk("ANMF", framePayload),
		makeWebpChunk("ANMF", framePayload),
		makeWebpChunk("ANMF", framePayload),
		makeWebpChunk("ANMF", framePayload),
		makeWebpChunk("ANMF", framePayload),
	)
	trimmed, ok := TrimAnimatedWebp(oversizedAnim, MaxAnimatedBytes-512)
	if !ok {
		t.Fatal("expected TrimAnimatedWebp to succeed")
	}
	if len(trimmed) > MaxAnimatedBytes-512 {
		t.Fatalf("trimmed size = %d exceeds cap %d", len(trimmed), MaxAnimatedBytes-512)
	}
	if gotFrames := CountWebpAnimFrames(trimmed); gotFrames != 4 {
		t.Fatalf("expected 4 kept frames, got %d", gotFrames)
	}

	rebranded, _, err := InjectWebpExif(trimmed, ExifMetadata{
		PackID:    "test",
		Publisher: "Sonicos",
		PackName:  "Sonicos Sound",
		Emojis:    []string{"🔊"},
	}, 512, 512)
	if err != nil {
		t.Fatalf("InjectWebpExif after TrimAnimatedWebp failed: %v", err)
	}
	if len(rebranded) > MaxAnimatedBytes {
		t.Fatalf("rebranded size = %d exceeds MaxAnimatedBytes %d", len(rebranded), MaxAnimatedBytes)
	}
}

func TestDubSoundSticker_RejectsOversizedSingleFrameAnimatedWebpCleanly(t *testing.T) {

	vp8x := make([]byte, 10)
	vp8x[0] = 0x02
	anim := make([]byte, 6)
	hugeSingleFrame := makeTestWebP(
		makeWebpChunk("VP8X", vp8x),
		makeWebpChunk("ANIM", anim),
		makeWebpChunk("ANMF", bytes.Repeat([]byte{0xBB}, 510<<10)),
	)
	if _, ok := TrimAnimatedWebp(hugeSingleFrame, MaxAnimatedBytes-512); ok {
		t.Fatal("expected single-frame >500 KiB animated WebP not to trim cleanly")
	}

	c := NewConverter("ffmpeg")
	_, err := c.DubSoundSticker(context.Background(), command.Attachment{Kind: command.MediaSticker, MIMEType: "image/webp"}, command.Attachment{Kind: command.MediaAudio, Data: []byte("x")})
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("expected empty attachment error, got %v", err)
	}
}
