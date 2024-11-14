// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package memedraw draws text on a tempate.
package memedraw

import (
	"fmt"
	"image"
	"image/draw"
	"image/gif"
	"log"
	"math"
	"runtime"
	"strings"
	"time"

	"github.com/creachadair/taskgroup"
	"github.com/fogleman/gg"
	"github.com/golang/freetype/truetype"
	"github.com/tailscale/tmemes"
	"golang.org/x/image/font"

	_ "embed"
)

// Preloaded font definition.
var (
	//go:embed Oswald-SemiBold.ttf
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

	backdrops := make([]*image.Paletted, len(img.Image))
	backdropReady := make([]chan struct{}, len(img.Image))
	for i := range img.Image {
		backdropReady[i] = make(chan struct{})
	}

	// Draw first frame's backdrop.
	backdrops[0] = image.NewPaletted(bounds, img.Image[0].Palette)
	draw.Draw(backdrops[0], bounds, image.NewUniform(img.Image[0].Palette[img.BackgroundIndex]), image.Point{}, draw.Src)
	close(backdropReady[0])

	g, run := taskgroup.New(nil).Limit(runtime.NumCPU())
	for i := 0; i < len(img.Image); i++ {
		i, frame := i, img.Image[i]
		run.Run(func() {
			pal := frame.Palette
			fb := frame.Bounds()

			// Block until the required background for this frame is already painted.
			<-backdropReady[i]

			dst := image.NewPaletted(bounds, pal)

			// Draw the backdrop.
			copy(dst.Pix, backdrops[i].Pix)

			// Draw the frame.
			draw.Draw(dst, fb, frame, fb.Min, draw.Over)

			// Sort out next frame's backdrop, unless we're on the final frame.
			if i != len(img.Image)-1 {
				switch img.Disposal[i] {
				case gif.DisposalBackground:
					// Restore background colour.
					backdrops[i+1] = backdrops[0]
				case gif.DisposalPrevious:
					// Keep the backdrops the same, i.e. discard whatever this frame drew.
					backdrops[i+1] = backdrops[i]
				case gif.DisposalNone:
					// Do not dispose of the frame, i.e. copy this frame to be next frame's backdrop.
					backdrops[i+1] = image.NewPaletted(bounds, pal)
					copy(backdrops[i+1].Pix, dst.Pix)
				default:
					backdrops[i+1] = backdrops[0]
				}
				close(backdropReady[i+1])
			}

			// Draw the text overlay.
			dc := gg.NewContext(bounds.Dx(), bounds.Dy())
			for _, f := range lineFrames {
				if f.visibleAt(i) {
					overlayTextOnImage(dc, f.frame(i), bounds)
				}
			}
			text := dc.Image()
			draw.Draw(dst, dst.Bounds(), text, text.Bounds().Min, draw.Over)
			img.Image[i] = dst
		})
	}
	g.Wait()

	log.Printf("Rendering complete: %v", time.Since(rStart).Round(time.Millisecond))
	return img
}
