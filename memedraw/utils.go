package memedraw

import (
	"math"

	"github.com/tailscale/tmemes"
)

// newFrames constructs a frame tracker for a text line given an animation with
// the specified frameCount.
func newFrames(frameCount int, line tmemes.TextLine) frames {
	na := len(line.Field)
	fpa := math.Ceil(float64(frameCount) / float64(na))
	start, end := 0, frameCount
	if line.Start > 0 {
		start = int(math.Ceil(line.Start * float64(frameCount)))
	}
	if line.End > line.Start {
		end = int(math.Ceil(line.End * float64(frameCount)))
	}
	return frames{
		line:          line,
		framesPerArea: int(fpa),
		start:         start,
		end:           end,
	}
}

// A frames value wraps a TextLine with the ability to figure out which of
// possibly-multiple positions should be rendered at a given frame index.
type frames struct {
	line          tmemes.TextLine
	framesPerArea int
	start, end    int
}

// visibleAt reports whether the text is visible at index i ≥ 0.
func (f frames) visibleAt(i int) bool {
	return f.start <= i && i <= f.end
}

// frame returns the frame information for index i ≥ 0.
func (f frames) frame(i int) frame {
	if len(f.line.Field) == 1 {
		return frame{f.line, 0, 0, 1}
	}
	pos := (i / f.framesPerArea) % len(f.line.Field)
	return frame{f.line, pos, i, f.framesPerArea}
}

// A frame wraps a single-frame view of a movable text line.  Call the Area
// method to get the current position for the line.
type frame struct {
	tmemes.TextLine
	pos, i, fpa int
}

func (f frame) area() tmemes.Area {
	cur := f.Field[f.pos]
	if !cur.Tween {
		return cur
	}
	if rem := f.i % f.fpa; rem != 0 {
		// Find the next area in sequence (not just the next frame).
		npos := ((f.i + f.fpa) / f.fpa) % len(f.Field)
		next := f.Field[npos]

		// Compute a linear interpolation and update the apparent position.
		// We have a copy, so it's safe to update in-place.
		dx := (next.X - cur.X) / float64(f.fpa)
		dy := (next.Y - cur.Y) / float64(f.fpa)
		cur.X += float64(rem) * dx
		cur.Y += float64(rem) * dy

	}
	return cur
}
