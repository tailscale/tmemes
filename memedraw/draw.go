// Package draw draws text on a tempate.
package memedraw

import (
	_ "embed"
	"fmt"
	"image"
	"image/draw"
	"image/gif"
	"log"
	"math"
	"strings"
	"time"

	"github.com/fogleman/gg"
	"github.com/golang/freetype/truetype"
	"github.com/tailscale/tmemes"
	"golang.org/x/image/font"
)

// Preloaded font definition.
var (
	//go:embed Oswald-Bold.ttf
	oswaldSemiBoldBytes []byte

	oswaldSemiBold *truetype.Font
)

func init() {
	var err error
	oswaldSemiBold, err = truetype.Parse(oswaldSemiBoldBytes)
	if err != nil {
		panic(fmt.Sprintf("Parsing font: %v", err))
	}
}

// fontForSize constructs a new font.Face for the specified point size.
func fontForSize(points int) font.Face {
	return truetype.NewFace(oswaldSemiBold, &truetype.Options{
		Size: float64(points),
	})
}

// fontSizeForImage computes a recommend font size in points for the given image.
func fontSizeForImage(img image.Image) int {
	const typeHeightFraction = 0.15
	points := int(math.Round((float64(img.Bounds().Dy()) * 0.75) * typeHeightFraction))
	return points
}

func oneForZero(v float64) float64 {
	if v == 0 {
		return 1
	}
	return v
}

// overlayTextOnImage paints the specified text line on a single image frame.
func overlayTextOnImage(dc *gg.Context, tl frame, bounds image.Rectangle) {
	text := strings.TrimSpace(tl.Text)
	if text == "" {
		return
	}

	fontSize := fontSizeForImage(bounds)
	font := fontForSize(fontSize)
	dc.SetFontFace(font)

	width := oneForZero(tl.Field[0].Width) * float64(bounds.Dx())
	lineSpacing := 1.25
	x := tl.area().X * float64(bounds.Dx())
	y := tl.area().Y * float64(bounds.Dy())
	ax := 0.5
	ay := 1.0
	fontHeight := dc.FontHeight()
	// Replicate part of the DrawStringWrapped logic so that we can draw the
	// text multiple times to create an outline effect.
	lines := dc.WordWrap(text, width)

	for len(lines) > 2 && fontSize > 6 {
		fontSize--
		font = fontForSize(fontSize)
		dc.SetFontFace(font)
		lines = dc.WordWrap(text, width)
	}

	// sync h formula with MeasureMultilineString
	h := float64(len(lines)) * fontHeight * lineSpacing
	h -= (lineSpacing - 1) * fontHeight
	y -= 0.5 * h

	for _, line := range lines {
		c := tl.StrokeColor
		dc.SetRGB(c.R(), c.G(), c.B())

		n := 6 // visible outline size
		for dy := -n; dy <= n; dy++ {
			for dx := -n; dx <= n; dx++ {
				if dx*dx+dy*dy >= n*n {
					// give it rounded corners
					continue
				}
				dc.DrawStringAnchored(line, x+float64(dx), y+float64(dy), ax, ay)
			}
		}

		c = tl.Color
		dc.SetRGB(c.R(), c.G(), c.B())

		dc.DrawStringAnchored(line, x, y, ax, ay)
		y += fontHeight * lineSpacing
	}
}

func Draw(srcImage image.Image, m *tmemes.Macro) image.Image {
	dc := gg.NewContext(srcImage.Bounds().Dx(), srcImage.Bounds().Dy())
	bounds := srcImage.Bounds()
	for _, tl := range m.TextOverlay {
		overlayTextOnImage(dc, newFrames(1, tl).frame(0), bounds)
	}

	alpha := image.NewNRGBA(bounds)
	draw.Draw(alpha, bounds, srcImage, bounds.Min, draw.Src)
	draw.Draw(alpha, bounds, dc.Image(), bounds.Min, draw.Over)
	return alpha
}

func DrawGIF(img *gif.GIF, m *tmemes.Macro) *gif.GIF {
	lineFrames := make([]frames, len(m.TextOverlay))
	for i, tl := range m.TextOverlay {
		lineFrames[i] = newFrames(len(img.Image), tl)
	}

	bounds := image.Rect(0, 0, img.Config.Width, img.Config.Height)
	rStart := time.Now()

	backdrop := image.NewPaletted(bounds, img.Image[0].Palette)
	for i, frame := range img.Image {
		fb := frame.Bounds()
		dst := image.NewPaletted(bounds, frame.Palette)
		// 1. Draw the backdrop.
		draw.Draw(dst, bounds, backdrop, image.ZP, draw.Over)
		// 2. Draw the frame.
		draw.Draw(dst, fb, frame, fb.Min, draw.Over)
		// 3. Draw the text.
		dc := gg.NewContext(bounds.Dx(), bounds.Dy())
		for _, f := range lineFrames {
			if f.visibleAt(i) {
				overlayTextOnImage(dc, f.frame(i), bounds)
			}
		}
		text := dc.Image()
		draw.Draw(dst, dst.Bounds(), text, text.Bounds().Min, draw.Over)
		img.Image[i] = dst

		// Work out the next frame's backdrop.
		switch img.Disposal[i] {
		case gif.DisposalBackground:
			// Restore background colour.
			draw.Draw(backdrop, bounds, image.NewUniform(frame.Palette[img.BackgroundIndex]), image.ZP, draw.Over)
		case gif.DisposalPrevious:
			// Keep the background the same, i.e. previous frame.
		case gif.DisposalNone:
			// Do not dispose of the frame.
			fallthrough
		default:
			draw.Draw(backdrop, fb, frame, fb.Min, draw.Over)
		}
	}
	log.Printf("Rendering complete [render %v]", time.Since(rStart).Round(time.Millisecond))
	return img
}