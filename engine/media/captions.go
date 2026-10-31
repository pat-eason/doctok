package media

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
)

// CaptionStyle controls burned-in TikTok-style captions.
type CaptionStyle struct {
	TextColor      string  `json:"text_color"`      // hex, e.g. #FFFFFF
	StrokeColor    string  `json:"stroke_color"`    // hex
	HighlightColor string  `json:"highlight_color"` // active-word color; "" = no per-word highlight
	Position       float64 `json:"position"`        // vertical center of caption band, fraction of height
	WordsPerChunk  int     `json:"words_per_chunk"`
	Uppercase      bool    `json:"uppercase"`
	FontScale      float64 `json:"font_scale"` // 1.0 = 7.2% of width
	TopBoxColor    string  `json:"top_box_color"`
	TopTextColor   string  `json:"top_text_color"`
	TopPosition    float64 `json:"top_position"` // fraction of height for the top overlay
	TopUppercase   bool    `json:"top_uppercase"`
	TopScale       float64 `json:"top_scale"` // 1.0 = 5% of width
}

// NoBox is a TopBoxColor value meaning "stroked text, no sticker box".
const NoBox = "none"

// DefaultCaptionStyle is a classic white-bold-with-black-stroke look.
func DefaultCaptionStyle() CaptionStyle {
	return CaptionStyle{
		TextColor: "#FFFFFF", StrokeColor: "#000000", HighlightColor: "#FFE135",
		Position: 0.68, WordsPerChunk: 3, Uppercase: true, FontScale: 1.0,
		TopBoxColor: "#FFFFFF", TopTextColor: "#111111", TopPosition: 0.11,
	}
}

func (s CaptionStyle) withDefaults() CaptionStyle {
	d := DefaultCaptionStyle()
	if s.TextColor == "" {
		s.TextColor = d.TextColor
	}
	if s.StrokeColor == "" {
		s.StrokeColor = d.StrokeColor
	}
	if s.Position <= 0 || s.Position >= 1 {
		s.Position = d.Position
	}
	if s.WordsPerChunk <= 0 {
		s.WordsPerChunk = d.WordsPerChunk
	}
	if s.FontScale <= 0 {
		s.FontScale = d.FontScale
	}
	if s.TopBoxColor == "" {
		s.TopBoxColor = d.TopBoxColor
	}
	if s.TopTextColor == "" {
		s.TopTextColor = d.TopTextColor
	}
	if s.TopPosition <= 0 || s.TopPosition >= 1 {
		s.TopPosition = d.TopPosition
	}
	if s.TopScale <= 0 {
		s.TopScale = 1
	}
	return s
}

// Cue is one timed caption image.
type Cue struct {
	Start, End float64
	Words      []string
	Active     int // index of highlighted word, -1 = none
}

// BuildCues splits dialogue into word chunks timed across [lead, dur-tail].
// When highlight is on, each word of each chunk gets its own cue.
func BuildCues(dialogue string, dur float64, style CaptionStyle) []Cue {
	style = style.withDefaults()
	words := strings.Fields(dialogue)
	if len(words) == 0 || dur <= 0 {
		return nil
	}
	lead, tail := 0.25, 0.35
	if dur < 2 {
		lead, tail = 0, 0
	}
	span := dur - lead - tail
	// Weight words by length so long words stay on screen longer.
	weights := make([]float64, len(words))
	total := 0.0
	for i, w := range words {
		weights[i] = 1 + float64(len([]rune(w)))*0.12
		total += weights[i]
	}
	starts := make([]float64, len(words)+1)
	t := lead
	for i := range words {
		starts[i] = t
		t += span * weights[i] / total
	}
	starts[len(words)] = t
	var cues []Cue
	for i := 0; i < len(words); i += style.WordsPerChunk {
		j := min(i+style.WordsPerChunk, len(words))
		chunk := words[i:j]
		if style.HighlightColor == "" {
			cues = append(cues, Cue{Start: starts[i], End: starts[j], Words: chunk, Active: -1})
			continue
		}
		for k := i; k < j; k++ {
			cues = append(cues, Cue{Start: starts[k], End: starts[k+1], Words: chunk, Active: k - i})
		}
	}
	// Last cue lingers to the end of the clip.
	if len(cues) > 0 {
		cues[len(cues)-1].End = dur
	}
	return cues
}

var (
	fontOnce sync.Once
	fontData *opentype.Font
	fontErr  error
)

// Preferred chunky sans fonts; falls back to embedded Go Bold.
var fontCandidates = []string{
	"/System/Library/Fonts/Supplemental/Arial Black.ttf",
	"/Library/Fonts/Arial Black.ttf",
	"/usr/share/fonts/truetype/msttcorefonts/Arial_Black.ttf",
	"/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf",
}

func loadFont() (*opentype.Font, error) {
	fontOnce.Do(func() {
		if p := os.Getenv("DOCTOK_FONT"); p != "" {
			fontCandidates = append([]string{p}, fontCandidates...)
		}
		for _, p := range fontCandidates {
			b, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			if f, err := opentype.Parse(b); err == nil {
				fontData = f
				return
			}
		}
		fontData, fontErr = opentype.Parse(gobold.TTF)
	})
	return fontData, fontErr
}

func parseHex(s string, fallback color.RGBA) color.RGBA {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	if len(s) == 3 {
		s = string([]byte{s[0], s[0], s[1], s[1], s[2], s[2]})
	}
	if len(s) != 6 && len(s) != 8 {
		return fallback
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return fallback
	}
	if len(s) == 6 {
		return color.RGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 255}
	}
	// RRGGBBAA → premultiplied RGBA.
	a := uint32(v & 0xff)
	pm := func(c uint32) uint8 { return uint8(c * a / 255) }
	return color.RGBA{pm(uint32(v>>24) & 0xff), pm(uint32(v>>16) & 0xff), pm(uint32(v>>8) & 0xff), uint8(a)}
}

// sanitize drops runes the font can't draw (emoji etc.).
func sanitize(f *opentype.Font, s string) string {
	var buf sfnt.Buffer
	var sb strings.Builder
	for _, r := range s {
		if unicode.IsSpace(r) {
			sb.WriteRune(' ')
			continue
		}
		idx, err := f.GlyphIndex(&buf, r)
		if err == nil && idx != 0 {
			sb.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(sb.String()), " ")
}

// RenderCue draws a transparent band image (width × bandH) with the caption
// centered; returns the image height so the caller can position it.
func RenderCue(path string, cue Cue, width int, style CaptionStyle) (int, error) {
	style = style.withDefaults()
	f, err := loadFont()
	if err != nil {
		return 0, err
	}
	size := float64(width) * 0.072 * style.FontScale
	face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return 0, err
	}
	defer face.Close()

	words := make([]string, 0, len(cue.Words))
	for _, w := range cue.Words {
		w = sanitize(f, w)
		if style.Uppercase {
			w = strings.ToUpper(w)
		}
		if w != "" {
			words = append(words, w)
		}
	}
	if len(words) == 0 {
		words = []string{" "}
	}
	maxW := int(float64(width) * 0.86)
	lines := wrapWords(face, words, maxW)
	lineH := int(math.Ceil(size * 1.18))
	stroke := int(math.Max(3, size*0.09))
	h := lineH*len(lines) + stroke*4
	img := image.NewRGBA(image.Rect(0, 0, width, h))

	textCol := parseHex(style.TextColor, color.RGBA{255, 255, 255, 255})
	strokeCol := parseHex(style.StrokeColor, color.RGBA{0, 0, 0, 255})
	hiCol := parseHex(style.HighlightColor, textCol)
	space := font.MeasureString(face, " ").Ceil()

	wordIdx := 0
	for li, line := range lines {
		lw := lineWidth(face, line, space)
		x := (width - lw) / 2
		y := stroke*2 + li*lineH + int(size*0.92)
		for _, w := range line {
			col := textCol
			if cue.Active >= 0 && wordIdx == cue.Active {
				col = hiCol
			}
			drawStroked(img, face, w, x, y, col, strokeCol, stroke)
			x += font.MeasureString(face, w).Ceil() + space
			wordIdx++
		}
	}
	return h, savePNG(path, img)
}

// RenderTopText draws a rounded "TikTok text" sticker band.
func RenderTopText(path, text string, width int, style CaptionStyle) (int, error) {
	style = style.withDefaults()
	f, err := loadFont()
	if err != nil {
		return 0, err
	}
	size := float64(width) * 0.05 * style.FontScale * style.TopScale
	face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return 0, err
	}
	defer face.Close()
	text = sanitize(f, text)
	if style.TopUppercase {
		text = strings.ToUpper(text)
	}
	if text == "" {
		return 0, fmt.Errorf("empty top text")
	}
	if strings.EqualFold(style.TopBoxColor, NoBox) {
		// Classic meme / rank text: stroked, no box.
		lines := wrapWords(face, strings.Fields(text), int(float64(width)*0.86))
		space := font.MeasureString(face, " ").Ceil()
		lineH := int(math.Ceil(size * 1.18))
		stroke := int(math.Max(3, size*0.09))
		h := lineH*len(lines) + stroke*4
		img := image.NewRGBA(image.Rect(0, 0, width, h))
		fill := parseHex(style.TopTextColor, color.RGBA{255, 255, 255, 255})
		strokeCol := parseHex(style.StrokeColor, color.RGBA{0, 0, 0, 255})
		for li, line := range lines {
			x := (width - lineWidth(face, line, space)) / 2
			y := stroke*2 + li*lineH + int(size*0.92)
			for _, w := range line {
				drawStroked(img, face, w, x, y, fill, strokeCol, stroke)
				x += font.MeasureString(face, w).Ceil() + space
			}
		}
		return h, savePNG(path, img)
	}
	space := font.MeasureString(face, " ").Ceil()
	lines := wrapWords(face, strings.Fields(text), int(float64(width)*0.78))
	lineH := int(math.Ceil(size * 1.25))
	padX, padY := int(size*0.6), int(size*0.45)
	h := lineH*len(lines) + padY*2
	img := image.NewRGBA(image.Rect(0, 0, width, h+8))
	box := parseHex(style.TopBoxColor, color.RGBA{255, 255, 255, 255})
	txt := parseHex(style.TopTextColor, color.RGBA{17, 17, 17, 255})
	for li, line := range lines {
		lw := lineWidth(face, line, space)
		x0 := (width-lw)/2 - padX
		y0 := li * lineH
		y1 := y0 + lineH + padY*2
		if li > 0 {
			y0 += padY
		}
		fillRounded(img, image.Rect(x0, y0, x0+lw+padX*2, y1), int(size*0.35), box)
	}
	for li, line := range lines {
		lw := lineWidth(face, line, space)
		x := (width - lw) / 2
		y := padY + li*lineH + int(size*0.95)
		for _, w := range line {
			d := font.Drawer{Dst: img, Src: image.NewUniform(txt), Face: face, Dot: fixed.P(x, y)}
			d.DrawString(w)
			x += font.MeasureString(face, w).Ceil() + space
		}
	}
	return h + 8, savePNG(path, img)
}

func wrapWords(face font.Face, words []string, maxW int) [][]string {
	space := font.MeasureString(face, " ").Ceil()
	var lines [][]string
	var cur []string
	curW := 0
	for _, w := range words {
		ww := font.MeasureString(face, w).Ceil()
		if len(cur) > 0 && curW+space+ww > maxW {
			lines = append(lines, cur)
			cur, curW = nil, 0
		}
		if len(cur) > 0 {
			curW += space
		}
		cur = append(cur, w)
		curW += ww
	}
	if len(cur) > 0 {
		lines = append(lines, cur)
	}
	return lines
}

func lineWidth(face font.Face, line []string, space int) int {
	w := 0
	for i, s := range line {
		if i > 0 {
			w += space
		}
		w += font.MeasureString(face, s).Ceil()
	}
	return w
}

func drawStroked(dst *image.RGBA, face font.Face, s string, x, y int, fill, stroke color.RGBA, r int) {
	st := image.NewUniform(stroke)
	steps := max(16, r*4)
	for i := 0; i < steps; i++ {
		a := 2 * math.Pi * float64(i) / float64(steps)
		dx, dy := int(math.Round(float64(r)*math.Cos(a))), int(math.Round(float64(r)*math.Sin(a)))
		d := font.Drawer{Dst: dst, Src: st, Face: face, Dot: fixed.P(x+dx, y+dy)}
		d.DrawString(s)
	}
	// Fill the stroke interior too (thick outline without gaps).
	for rr := 1; rr < r; rr++ {
		for _, o := range [][2]int{{rr, 0}, {-rr, 0}, {0, rr}, {0, -rr}} {
			d := font.Drawer{Dst: dst, Src: st, Face: face, Dot: fixed.P(x+o[0], y+o[1])}
			d.DrawString(s)
		}
	}
	d := font.Drawer{Dst: dst, Src: image.NewUniform(fill), Face: face, Dot: fixed.P(x, y)}
	d.DrawString(s)
}

func fillRounded(img *image.RGBA, r image.Rectangle, rad int, c color.RGBA) {
	r = r.Intersect(img.Bounds())
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			cx, cy := 0, 0
			switch {
			case x < r.Min.X+rad && y < r.Min.Y+rad:
				cx, cy = r.Min.X+rad, r.Min.Y+rad
			case x >= r.Max.X-rad && y < r.Min.Y+rad:
				cx, cy = r.Max.X-rad-1, r.Min.Y+rad
			case x < r.Min.X+rad && y >= r.Max.Y-rad:
				cx, cy = r.Min.X+rad, r.Max.Y-rad-1
			case x >= r.Max.X-rad && y >= r.Max.Y-rad:
				cx, cy = r.Max.X-rad-1, r.Max.Y-rad-1
			default:
				img.SetRGBA(x, y, c)
				continue
			}
			if (x-cx)*(x-cx)+(y-cy)*(y-cy) <= rad*rad {
				img.SetRGBA(x, y, c)
			}
		}
	}
}

func savePNG(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(f, img); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
