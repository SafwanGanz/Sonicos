package sticker

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

type ExifMetadata struct {
	PackID    string   `json:"sticker-pack-id"`
	Publisher string   `json:"sticker-pack-publisher"`
	PackName  string   `json:"sticker-pack-name"`
	Emojis    []string `json:"emojis"`
}

func newPackID(prefix string) string {
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return prefix + "00000000"
	}
	return prefix + hex.EncodeToString(raw[:])
}

func InjectWebpExif(webpBuf []byte, meta ExifMetadata, width, height int) ([]byte, uint32, error) {
	if len(webpBuf) < 12 || string(webpBuf[:4]) != "RIFF" || string(webpBuf[8:12]) != "WEBP" {
		return nil, 0, errors.New("invalid WebP buffer: missing RIFF/WEBP header")
	}
	if width <= 0 {
		width = 512
	}
	if height <= 0 {
		height = 512
	}

	var vp8xChunk []byte
	var hasAnim bool
	var hasAlpha bool
	var otherChunks [][]byte

	offset := 12
	for offset+8 <= len(webpBuf) {
		fourcc := string(webpBuf[offset : offset+4])
		size := int(binary.LittleEndian.Uint32(webpBuf[offset+4 : offset+8]))
		if size < 0 {
			return nil, 0, fmt.Errorf("invalid WebP chunk size for %q", fourcc)
		}
		payloadStart := offset + 8
		payloadEnd := payloadStart + size
		if payloadEnd > len(webpBuf) {
			payloadEnd = len(webpBuf)
		}
		chunkPayload := webpBuf[payloadStart:payloadEnd]
		padded := size + (size & 1)
		nextOffset := payloadStart + padded
		if nextOffset <= offset {
			break
		}
		offset = nextOffset

		if fourcc == "EXIF" {
			continue
		}

		paddedLen := len(chunkPayload) + (len(chunkPayload) & 1)
		chunkBuf := make([]byte, 8+paddedLen)
		copy(chunkBuf[0:4], fourcc)
		binary.LittleEndian.PutUint32(chunkBuf[4:8], uint32(len(chunkPayload)))
		copy(chunkBuf[8:], chunkPayload)

		if fourcc == "ANIM" || fourcc == "ANMF" {
			hasAnim = true
		}
		if fourcc == "ALPH" {
			hasAlpha = true
		} else if fourcc == "VP8L" && len(chunkPayload) >= 5 {
			bits := binary.LittleEndian.Uint32(chunkPayload[1:5])
			if ((bits >> 28) & 1) == 1 {
				hasAlpha = true
			}
		}

		if fourcc == "VP8X" {
			if len(chunkPayload) >= 10 {
				if (chunkBuf[8] & 0x10) != 0 {
					hasAlpha = true
				}
				if (chunkBuf[8] & 0x02) != 0 {
					hasAnim = true
				}
				chunkBuf[8] |= 0x08
			}
			vp8xChunk = chunkBuf
		} else {
			otherChunks = append(otherChunks, chunkBuf)
		}
	}

	if vp8xChunk == nil {
		vp8xChunk = make([]byte, 18)
		copy(vp8xChunk[0:4], "VP8X")
		binary.LittleEndian.PutUint32(vp8xChunk[4:8], 10)
		var flags uint32 = 0x08
		if hasAlpha {
			flags |= 0x10
		}
		if hasAnim {
			flags |= 0x02
		}
		binary.LittleEndian.PutUint32(vp8xChunk[8:12], flags)
		w1 := width - 1
		h1 := height - 1
		vp8xChunk[12] = byte(w1 & 0xff)
		vp8xChunk[13] = byte((w1 >> 8) & 0xff)
		vp8xChunk[14] = byte((w1 >> 16) & 0xff)
		vp8xChunk[15] = byte(h1 & 0xff)
		vp8xChunk[16] = byte((h1 >> 8) & 0xff)
		vp8xChunk[17] = byte((h1 >> 16) & 0xff)
	}

	if len(meta.Emojis) == 0 {
		meta.Emojis = []string{"✨"}
	}
	jsonBuf, err := json.Marshal(meta)
	if err != nil {
		return nil, 0, fmt.Errorf("marshal EXIF metadata: %w", err)
	}

	tiffHeader := make([]byte, 26)
	copy(tiffHeader[0:2], "MM")
	binary.BigEndian.PutUint16(tiffHeader[2:4], 0x002a)
	binary.BigEndian.PutUint32(tiffHeader[4:8], 8)
	binary.BigEndian.PutUint16(tiffHeader[8:10], 1)
	binary.BigEndian.PutUint16(tiffHeader[10:12], 0x5741)
	binary.BigEndian.PutUint16(tiffHeader[12:14], 7)
	binary.BigEndian.PutUint32(tiffHeader[14:18], uint32(len(jsonBuf)))
	binary.BigEndian.PutUint32(tiffHeader[18:22], 0x1a)
	binary.BigEndian.PutUint32(tiffHeader[22:26], 0)

	exifPayloadLen := len(tiffHeader) + len(jsonBuf)
	exifPaddedLen := exifPayloadLen + (exifPayloadLen & 1)
	exifChunk := make([]byte, 8+exifPaddedLen)
	copy(exifChunk[0:4], "EXIF")
	binary.LittleEndian.PutUint32(exifChunk[4:8], uint32(exifPayloadLen))
	copy(exifChunk[8:], tiffHeader)
	copy(exifChunk[8+len(tiffHeader):], jsonBuf)

	bodyLen := len(vp8xChunk)
	for _, chunk := range otherChunks {
		bodyLen += len(chunk)
	}
	exifOffset := uint32(12 + bodyLen)

	out := make([]byte, 12+bodyLen+len(exifChunk))
	copy(out[0:4], "RIFF")
	binary.LittleEndian.PutUint32(out[4:8], uint32(4+bodyLen+len(exifChunk)))
	copy(out[8:12], "WEBP")

	pos := 12
	copy(out[pos:], vp8xChunk)
	pos += len(vp8xChunk)
	for _, chunk := range otherChunks {
		copy(out[pos:], chunk)
		pos += len(chunk)
	}
	copy(out[pos:], exifChunk)

	return out, exifOffset, nil
}

func FindWebpExifOffset(webp []byte) uint32 {
	if len(webp) < 12 || string(webp[:4]) != "RIFF" || string(webp[8:12]) != "WEBP" {
		return 0
	}
	off := 12
	for off+8 <= len(webp) {
		fourcc := string(webp[off : off+4])
		size := int(binary.LittleEndian.Uint32(webp[off+4 : off+8]))
		if fourcc == "EXIF" {
			return uint32(off)
		}
		if size < 0 {
			return 0
		}
		padded := size + (size & 1)
		next := off + 8 + padded
		if next <= off || next > len(webp) {
			return 0
		}
		off = next
	}
	return 0
}

func CountWebpAnimFrames(webp []byte) int {
	if len(webp) < 12 || string(webp[:4]) != "RIFF" || string(webp[8:12]) != "WEBP" {
		return 0
	}
	count := 0
	off := 12
	for off+8 <= len(webp) {
		fourcc := string(webp[off : off+4])
		size := int(binary.LittleEndian.Uint32(webp[off+4 : off+8]))
		if fourcc == "ANMF" {
			count++
		}
		if size < 0 {
			break
		}
		padded := size + (size & 1)
		next := off + 8 + padded
		if next <= off || next > len(webp) {
			break
		}
		off = next
	}
	return count
}

func HasWebpAnimChunk(webp []byte) bool {
	if len(webp) < 12 || string(webp[:4]) != "RIFF" || string(webp[8:12]) != "WEBP" {
		return false
	}
	off := 12
	for off+8 <= len(webp) {
		fourcc := string(webp[off : off+4])
		size := int(binary.LittleEndian.Uint32(webp[off+4 : off+8]))
		if fourcc == "ANIM" || fourcc == "ANMF" {
			return true
		}
		if fourcc == "VP8X" && size >= 1 && off+8 < len(webp) && (webp[off+8]&0x02) != 0 {
			return true
		}
		if size < 0 {
			break
		}
		padded := size + (size & 1)
		next := off + 8 + padded
		if next <= off || next > len(webp) {
			break
		}
		off = next
	}
	return false
}

func TrimAnimatedWebp(webp []byte, maxBytes int) ([]byte, bool) {
	if len(webp) < 12 || string(webp[:4]) != "RIFF" || string(webp[8:12]) != "WEBP" {
		return nil, false
	}
	var headerChunks [][]byte
	var frameChunks [][]byte

	off := 12
	for off+8 <= len(webp) {
		fourcc := string(webp[off : off+4])
		size := int(binary.LittleEndian.Uint32(webp[off+4 : off+8]))
		if size < 0 {
			return nil, false
		}
		padded := size + (size & 1)
		end := off + 8 + padded
		if end <= off || end > len(webp) {
			break
		}
		chunk := make([]byte, end-off)
		copy(chunk, webp[off:end])
		off = end

		if fourcc == "EXIF" {
			continue
		}
		if fourcc == "ANMF" {
			frameChunks = append(frameChunks, chunk)
		} else {
			headerChunks = append(headerChunks, chunk)
		}
	}
	if len(frameChunks) == 0 {
		return nil, false
	}

	totalLen := 12
	for _, c := range headerChunks {
		totalLen += len(c)
	}
	kept := 0
	for _, f := range frameChunks {
		if totalLen+len(f) > maxBytes {
			break
		}
		totalLen += len(f)
		kept++
	}
	if kept == 0 {
		return nil, false
	}

	out := make([]byte, 12, totalLen)
	copy(out[0:4], "RIFF")
	binary.LittleEndian.PutUint32(out[4:8], uint32(totalLen-8))
	copy(out[8:12], "WEBP")
	for _, c := range headerChunks {
		out = append(out, c...)
	}
	for i := range kept {
		out = append(out, frameChunks[i]...)
	}
	return out, true
}
