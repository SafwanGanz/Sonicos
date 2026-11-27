package welcomecard

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/fogleman/gg"
	"github.com/golang/freetype/truetype"
	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	_ "golang.org/x/image/webp"
)

type Theme struct {
	Background string
	Primary    string
	Secondary  string
	Text       string
}

func DefaultTheme() Theme {
	return Theme{Background: "#FFFFFF", Primary: "#009DFF", Secondary: "#65C8FF"}
}

type Options struct {
	Width       int
	Height      int
	Scale       int
	Username    string
	ServerName  string
	WelcomeText string
	AvatarImage image.Image
	AvatarBytes []byte
	AvatarURL   string
	Theme       Theme
	FontRegular []byte
	FontBold    []byte
	HTTPClient  *http.Client
	OnAvatarError func(err error)
}

const (
	baseW       = 340.0
	baseH       = 180.0
	supersample = 4
)

func GeneratePNG(o Options) ([]byte, error) {
	img, err := Generate(o)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("encode png: %w", err)
	}
	return buf.Bytes(), nil
}

func Generate(o Options) (image.Image, error) {
	o = withDefaults(o)
	if o.Width < 120 || o.Height < 80 || o.Width > 4000 || o.Height > 4000 {
		return nil, fmt.Errorf("unsupported canvas size %dx%d", o.Width, o.Height)
	}

	fonts, err := loadFonts(o.FontRegular, o.FontBold)
	if err != nil {
		return nil, err
	}
	avatar := resolveAvatar(o)

	S := float64(o.Scale * supersample)
	W := float64(o.Width) * S
	H := float64(o.Height) * S
	dc := gg.NewContext(int(W), int(H))

	dc.SetHexColor(o.Theme.Background)
	dc.Clear()

	drawWaves(dc, W, H, o.Theme)

	u := math.Min(W/baseW, H/baseH)
	offY := (H - baseH*u) / 2
	cx := W / 2

	ringW := 3.0 * u
	outerR := 36.0 * u
	avatarR := outerR - ringW
	cy := offY + 22.0*u + outerR

	drawAvatar(dc, avatar, cx, cy, avatarR, ringW, o.Theme)

	textCol := o.Theme.Text
	if textCol == "" {
		textCol = o.Theme.Primary
	}
	maxTextW := W * 0.56

	type line struct {
		text     string
		font     *truetype.Font
		size     float64
		min      float64
		baseline float64
	}
	lines := []line{
		{strings.ToUpper(o.WelcomeText), fonts.bold, 18, 11, 117},
		{o.Username, fonts.bold, 15, 8, 136},
		{o.ServerName, fonts.regular, 12, 8, 153},
	}
	dc.SetHexColor(textCol)
	for _, l := range lines {
		if strings.TrimSpace(l.text) == "" {
			continue
		}
		drawFittedText(dc, l.font, l.text, cx, offY+l.baseline*u, maxTextW, l.size*u, l.min*u)
	}

	out := image.NewRGBA(image.Rect(0, 0, o.Width*o.Scale, o.Height*o.Scale))
	xdraw.CatmullRom.Scale(out, out.Bounds(), dc.Image(), dc.Image().Bounds(), xdraw.Src, nil)
	return out, nil
}

type wave struct {
	topX, leftY float64
	c           [2][6]float64
	usePrimary  bool
}

var cornerWaves = []wave{
	{
		topX: 0.50, leftY: 0.66, usePrimary: false,
		c: [2][6]float64{
			{0.40, 0.11, 0.27, 0.13, 0.18, 0.28},
			{0.11, 0.40, 0.05, 0.50, 0.00, 0.66},
		},
	},
	{
		topX: 0.36, leftY: 0.50, usePrimary: true,
		c: [2][6]float64{
			{0.28, 0.10, 0.16, 0.10, 0.10, 0.23},
			{0.05, 0.33, 0.03, 0.40, 0.00, 0.50},
		},
	},
	{
		topX: 0.22, leftY: 0.30, usePrimary: false,
		c: [2][6]float64{
			{0.16, 0.06, 0.08, 0.07, 0.05, 0.15},
			{0.03, 0.21, 0.02, 0.25, 0.00, 0.30},
		},
	},
}

func drawWaves(dc *gg.Context, W, H float64, t Theme) {
	const k = 0.70
	paint := func() {
		for _, w := range cornerWaves {
			if w.usePrimary {
				dc.SetHexColor(t.Primary)
			} else {
				dc.SetHexColor(t.Secondary)
			}
			dc.NewSubPath()
			dc.MoveTo(0, 0)
			dc.LineTo(k*w.topX*W, 0)
			for _, c := range w.c {
				dc.CubicTo(k*c[0]*W, k*c[1]*H, k*c[2]*W, k*c[3]*H, k*c[4]*W, k*c[5]*H)
			}
			dc.ClosePath()
			dc.Fill()
		}
	}

	paint()

	dc.Push()
	dc.Translate(W, H)
	dc.Rotate(math.Pi)
	paint()
	dc.Pop()
}

func resolveAvatar(o Options) image.Image {
	report := func(err error) {
		if err != nil && o.OnAvatarError != nil {
			o.OnAvatarError(err)
		}
	}
	if o.AvatarImage != nil {
		return o.AvatarImage
	}
	if len(o.AvatarBytes) > 0 {
		img, _, err := image.Decode(bytes.NewReader(o.AvatarBytes))
		if err == nil {
			return img
		}
		report(fmt.Errorf("decode avatar bytes: %w", err))
	}
	if o.AvatarURL != "" {
		img, err := fetchAvatar(o.HTTPClient, o.AvatarURL)
		if err == nil {
			return img
		}
		report(err)
	}
	return defaultAvatar()
}

func fetchAvatar(client *http.Client, url string) (image.Image, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("avatar url: %w", err)
	}
	req.Header.Set("User-Agent", "welcomecard/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch avatar: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch avatar: HTTP %d", resp.StatusCode)
	}
	img, _, err := image.Decode(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("decode avatar: %w", err)
	}
	return img, nil
}

func defaultAvatar() image.Image {
	const n = 256
	dc := gg.NewContext(n, n)
	dc.SetHexColor("#2C2F33")
	dc.Clear()
	dc.SetHexColor("#99AAB5")
	dc.DrawCircle(n/2, 104, 46)
	dc.Fill()
	dc.DrawEllipse(n/2, 262, 92, 84)
	dc.Fill()
	return dc.Image()
}

func drawAvatar(dc *gg.Context, src image.Image, cx, cy, r, ringW float64, t Theme) {
	d := int(math.Round(2 * r))
	if d < 2 {
		d = 2
	}
	b := src.Bounds()
	side := b.Dx()
	if b.Dy() < side {
		side = b.Dy()
	}
	crop := image.Rect(
		b.Min.X+(b.Dx()-side)/2, b.Min.Y+(b.Dy()-side)/2,
		b.Min.X+(b.Dx()-side)/2+side, b.Min.Y+(b.Dy()-side)/2+side,
	)
	scaled := image.NewRGBA(image.Rect(0, 0, d, d))
	xdraw.CatmullRom.Scale(scaled, scaled.Bounds(), src, crop, xdraw.Src, nil)

	left := int(math.Round(cx)) - d/2
	top := int(math.Round(cy)) - d/2
	ccx, ccy, cr := float64(left)+float64(d)/2, float64(top)+float64(d)/2, float64(d)/2

	if hasTransparency(scaled) {
		dc.SetHexColor("#23272A")
		dc.DrawCircle(ccx, ccy, cr)
		dc.Fill()
	}

	dc.Push()
	dc.DrawCircle(ccx, ccy, cr)
	dc.Clip()
	dc.DrawImage(scaled, left, top)
	dc.ResetClip()
	dc.Pop()

	dc.SetHexColor(t.Primary)
	dc.SetLineWidth(ringW)
	dc.DrawCircle(ccx, ccy, cr+ringW/2)
	dc.Stroke()
}

func hasTransparency(img *image.RGBA) bool {
	for i := 3; i < len(img.Pix); i += 4 {
		if img.Pix[i] < 250 {
			return true
		}
	}
	return false
}

type fontSet struct{ regular, bold *truetype.Font }

func loadFonts(reg, bold []byte) (*fontSet, error) {
	if len(reg) == 0 {
		reg = goregular.TTF
	}
	if len(bold) == 0 {
		bold = gobold.TTF
	}
	r, err := truetype.Parse(reg)
	if err != nil {
		return nil, fmt.Errorf("parse regular font: %w", err)
	}
	b, err := truetype.Parse(bold)
	if err != nil {
		return nil, fmt.Errorf("parse bold font: %w", err)
	}
	return &fontSet{regular: r, bold: b}, nil
}

func newFace(f *truetype.Font, px float64) font.Face {
	return truetype.NewFace(f, &truetype.Options{Size: px, DPI: 72, Hinting: font.HintingNone})
}

func drawFittedText(dc *gg.Context, f *truetype.Font, text string, cx, baseline, maxW, startPx, minPx float64) {
	text = sanitize(text)
	px := startPx
	face := newFace(f, px)
	dc.SetFontFace(face)
	w, _ := dc.MeasureString(text)

	for w > maxW && px > minPx {
		px = math.Max(minPx, px-startPx*0.03)
		face = newFace(f, px)
		dc.SetFontFace(face)
		w, _ = dc.MeasureString(text)
	}
	if w > maxW {
		runes := []rune(text)
		for len(runes) > 1 {
			runes = runes[:len(runes)-1]
			cand := strings.TrimRightFunc(string(runes), unicode.IsSpace) + "\u2026"
			if cw, _ := dc.MeasureString(cand); cw <= maxW {
				text, w = cand, cw
				break
			}
		}
	}
	dc.DrawString(text, cx-w/2, baseline)
}

func sanitize(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

func withDefaults(o Options) Options {
	if o.Width == 0 {
		o.Width = int(baseW)
	}
	if o.Height == 0 {
		o.Height = int(baseH)
	}
	if o.Scale < 1 {
		o.Scale = 1
	}
	if o.WelcomeText == "" {
		o.WelcomeText = "Welcome"
	}
	if strings.TrimSpace(o.Username) == "" {
		o.Username = "Member"
	}
	d := DefaultTheme()
	if o.Theme.Background == "" {
		o.Theme.Background = d.Background
	}
	if o.Theme.Primary == "" {
		o.Theme.Primary = d.Primary
	}
	if o.Theme.Secondary == "" {
		o.Theme.Secondary = d.Secondary
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	return o
}

var _ color.Color