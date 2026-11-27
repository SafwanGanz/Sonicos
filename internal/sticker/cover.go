package sticker

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
)

func renderAudioCoverPNG(waveform []byte) ([]byte, error) {
	const size = 512
	const pad = 28
	const radius = 44

	img := image.NewNRGBA(image.Rect(0, 0, size, size))

	bg := color.NRGBA{R: 15, G: 23, B: 42, A: 245}
	border := color.NRGBA{R: 37, G: 211, B: 102, A: 180}
	inner := color.NRGBA{R: 30, G: 41, B: 59, A: 255}

	for y := pad; y < size-pad; y++ {
		for x := pad; x < size-pad; x++ {
			dist := roundedRectDistance(x, y, pad, pad, size-pad-1, size-pad-1, radius)
			if dist > 0.5 {
				continue
			}
			c := bg
			if dist > -3.0 {
				c = border
			} else if y < pad+140 && dist < -4.0 {

				c = color.NRGBA{R: 20, G: 31, B: 53, A: 248}
			}

			if dist > -0.5 {
				alpha := uint8(math.Round(float64(c.A) * (0.5 - dist)))
				c.A = alpha
			}
			img.SetNRGBA(x, y, c)
		}
	}

	drawCircle(img, 256, 134, 48, inner)
	drawCircleRing(img, 256, 134, 48, 3, color.NRGBA{R: 37, G: 211, B: 102, A: 230})
	drawSpeakerGlyph(img, 256, 134, color.NRGBA{R: 248, G: 250, B: 252, A: 255})

	const bars = 32
	const barW = 8
	const gap = 4
	totalW := bars*barW + (bars-1)*gap
	startX := (size - totalW) / 2
	centerY := 328

	for i := range bars {
		amp := 40
		if len(waveform) >= 64 {
			amp = (int(waveform[i*2]) + int(waveform[i*2+1])) / 2
		} else if len(waveform) > 0 {
			amp = int(waveform[i%len(waveform)])
		}
		if amp < 8 {
			amp = 8
		}
		if amp > 100 {
			amp = 100
		}
		barH := 18 + (amp*168)/100
		x0 := startX + i*(barW+gap)
		y0 := centerY - barH/2
		y1 := y0 + barH
		barCol := color.NRGBA{
			R: uint8(32 + (i*18)/bars),
			G: uint8(205 + (i*30)/bars),
			B: uint8(102 + (i*55)/bars),
			A: 255,
		}
		fillRoundedBar(img, x0, y0, x0+barW-1, y1, 4, barCol)
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func roundedRectDistance(x, y, x0, y0, x1, y1, r int) float64 {
	cx := math.Max(float64(x0+r), math.Min(float64(x), float64(x1-r)))
	cy := math.Max(float64(y0+r), math.Min(float64(y), float64(y1-r)))
	if x >= x0+r && x <= x1-r {
		dy := math.Max(float64(y0-y), float64(y-y1))
		if y >= y0 && y <= y1 {
			return -math.Min(float64(y-y0), float64(y1-y))
		}
		return dy
	}
	if y >= y0+r && y <= y1-r {
		if x >= x0 && x <= x1 {
			return -math.Min(float64(x-x0), float64(x1-x))
		}
		return math.Max(float64(x0-x), float64(x-x1))
	}
	dx := float64(x) - cx
	dy := float64(y) - cy
	return math.Hypot(dx, dy) - float64(r)
}

func fillRoundedBar(img *image.NRGBA, x0, y0, x1, y1, r int, c color.NRGBA) {
	for y := y0; y <= y1; y++ {
		for x := x0; x <= x1; x++ {
			if roundedRectDistance(x, y, x0, y0, x1, y1, r) <= 0 {
				img.SetNRGBA(x, y, c)
			}
		}
	}
}

func drawCircle(img *image.NRGBA, cx, cy, r int, c color.NRGBA) {
	r2 := r * r
	for y := cy - r; y <= cy+r; y++ {
		for x := cx - r; x <= cx+r; x++ {
			dx := x - cx
			dy := y - cy
			if dx*dx+dy*dy <= r2 {
				img.SetNRGBA(x, y, c)
			}
		}
	}
}

func drawCircleRing(img *image.NRGBA, cx, cy, r, thickness int, c color.NRGBA) {
	outer := float64(r)
	inner := float64(r - thickness)
	for y := cy - r; y <= cy+r; y++ {
		for x := cx - r; x <= cx+r; x++ {
			d := math.Hypot(float64(x-cx), float64(y-cy))
			if d <= outer && d >= inner {
				img.SetNRGBA(x, y, c)
			}
		}
	}
}

func drawSpeakerGlyph(img *image.NRGBA, cx, cy int, c color.NRGBA) {

	fillRoundedBar(img, cx-22, cy-10, cx-10, cy+10, 2, c)
	for x := cx - 10; x <= cx+2; x++ {
		span := 10 + ((x - (cx - 10)) * 10 / 12)
		for y := cy - span; y <= cy+span; y++ {
			img.SetNRGBA(x, y, c)
		}
	}

	for _, arcR := range []float64{13, 21} {
		for y := cy - int(arcR) - 2; y <= cy+int(arcR)+2; y++ {
			for x := cx + 4; x <= cx+int(arcR)+4; x++ {
				dx := float64(x - cx)
				dy := float64(y - cy)
				d := math.Hypot(dx, dy)
				if math.Abs(d-arcR) <= 1.8 && math.Abs(dy) <= dx*0.95 {
					img.SetNRGBA(x, y, c)
				}
			}
		}
	}
}
