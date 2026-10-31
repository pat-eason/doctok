package media

import (
	"image"
	"image/color"
	"math"
	"strings"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// RenderDocCard draws a full-frame "screenshot of the doc" background used
// behind chroma-keyed creators: a dark gradient with a white doc page showing
// the heading and the quoted excerpt in the upper half of the frame.
func RenderDocCard(path, heading, quote string, width, height int, accent string) error {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	top := color.RGBA{24, 20, 46, 255}
	bot := color.RGBA{60, 16, 70, 255}
	for y := 0; y < height; y++ {
		t := float64(y) / float64(height)
		c := color.RGBA{lerp(top.R, bot.R, t), lerp(top.G, bot.G, t), lerp(top.B, bot.B, t), 255}
		for x := 0; x < width; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	monoF, err := opentype.Parse(gomono.TTF)
	if err != nil {
		return err
	}
	boldF, err := opentype.Parse(gobold.TTF)
	if err != nil {
		return err
	}
	hSize := float64(width) * 0.045
	bSize := float64(width) * 0.036
	hFace, _ := opentype.NewFace(boldF, &opentype.FaceOptions{Size: hSize, DPI: 72, Hinting: font.HintingFull})
	bFace, _ := opentype.NewFace(monoF, &opentype.FaceOptions{Size: bSize, DPI: 72, Hinting: font.HintingFull})
	defer hFace.Close()
	defer bFace.Close()

	margin := int(float64(width) * 0.06)
	pad := int(float64(width) * 0.05)
	cardX0, cardX1 := margin, width-margin
	innerW := cardX1 - cardX0 - pad*2
	hLines := wrapWords(hFace, strings.Fields(sanitize(boldF, heading)), innerW)
	bLines := wrapWords(bFace, strings.Fields(sanitize(monoF, quote)), innerW)
	if len(bLines) > 12 {
		bLines = append(bLines[:11], []string{"…"})
	}
	hLH := int(math.Ceil(hSize * 1.3))
	bLH := int(math.Ceil(bSize * 1.45))
	barH := int(float64(width) * 0.05)
	cardH := barH + pad + len(hLines)*hLH + pad/2 + len(bLines)*bLH + pad
	cardY0 := int(float64(height) * 0.17)
	if maxH := int(float64(height) * 0.33); cardH > maxH {
		cardH = maxH
	}
	// Page + window chrome.
	fillRounded(img, image.Rect(cardX0, cardY0, cardX1, cardY0+cardH), 28, color.RGBA{250, 250, 247, 255})
	fillRounded(img, image.Rect(cardX0, cardY0, cardX1, cardY0+barH), 28, color.RGBA{232, 232, 228, 255})
	for i, c := range []color.RGBA{{255, 95, 87, 255}, {254, 188, 46, 255}, {40, 200, 64, 255}} {
		cx := cardX0 + pad + i*int(float64(barH)*0.7)
		cy := cardY0 + barH/2
		r := barH / 5
		for y := cy - r; y <= cy+r; y++ {
			for x := cx - r; x <= cx+r; x++ {
				if (x-cx)*(x-cx)+(y-cy)*(y-cy) <= r*r {
					img.SetRGBA(x, y, c)
				}
			}
		}
	}
	acc := parseHex(accent, color.RGBA{0, 229, 255, 255})
	y := cardY0 + barH + pad
	for _, line := range hLines {
		x := cardX0 + pad
		d := font.Drawer{Dst: img, Src: image.NewUniform(color.RGBA{20, 20, 24, 255}), Face: hFace, Dot: fixed.P(x, y+int(hSize))}
		d.DrawString(strings.Join(line, " "))
		y += hLH
	}
	// Accent highlight bar beside the quote, like a doc callout.
	quoteTop := y + pad/4
	y += pad / 2
	for _, line := range bLines {
		if y+bLH > cardY0+cardH-pad/2 {
			break
		}
		d := font.Drawer{Dst: img, Src: image.NewUniform(color.RGBA{40, 40, 48, 255}), Face: bFace, Dot: fixed.P(cardX0+pad+16, y+int(bSize))}
		d.DrawString(strings.Join(line, " "))
		y += bLH
	}
	for yy := quoteTop; yy < y; yy++ {
		for xx := cardX0 + pad - 4; xx < cardX0+pad+4; xx++ {
			img.SetRGBA(xx, yy, acc)
		}
	}
	return savePNG(path, img)
}

func lerp(a, b uint8, t float64) uint8 { return uint8(float64(a) + (float64(b)-float64(a))*t) }
