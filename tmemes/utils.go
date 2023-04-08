package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/creachadair/mds/slice"
	"github.com/tailscale/tmemes"
	"golang.org/x/exp/slices"
)

// sortMacros sorts a slice of macros in-place by the specified sorting key.
// The only possible error is if the sort key is not understood.
func sortMacros(key string, ms []*tmemes.Macro) error {
	// Check for sorting order.
	switch key {
	case "", "default", "id":
		// nothing to do, this is the order we get from the database
	case "recent":
		sortMacrosByRecency(ms)
	case "popular":
		sortMacrosByPopularity(ms)
	case "top-popular":
		top := slice.Partition(ms, func(m *tmemes.Macro) bool {
			return time.Since(m.CreatedAt) < 1*time.Hour
		})
		rest := ms[len(top):]
		sortMacrosByRecency(top)
		sortMacrosByPopularity(rest)
	default:
		return fmt.Errorf("invalid sort order %q", key)
	}
	return nil
}

func sortMacrosByRecency(ms []*tmemes.Macro) {
	slices.SortFunc(ms, func(a, b *tmemes.Macro) bool {
		return a.CreatedAt.After(b.CreatedAt)
	})
}

func sortMacrosByPopularity(ms []*tmemes.Macro) {
	// TODO: what should the definition of this be?
	slices.SortFunc(ms, func(a, b *tmemes.Macro) bool {
		da := a.Upvotes - a.Downvotes
		db := b.Upvotes - b.Downvotes
		if da == db {
			return a.CreatedAt.After(b.CreatedAt)
		}
		return da > db
	})
}

// parsePageOptions parses "page" and "count" query parameters from r if they
// are present. If they are present, they give the page > 0 and count > 0 that
// the endpoint should return. Otherwise, page < 0 and count = 0. If the count
// parameter is not specified or is 0, defaultCount is returned.
// It is an error if these parameters are present but invalid.
func parsePageOptions(r *http.Request, defaultCount int) (page, count int, _ error) {
	pageStr := r.FormValue("page")
	if pageStr == "" {
		return -1, 0, nil // pagination not requested (ignore count)
	}
	page, err := strconv.Atoi(pageStr)
	if err != nil {
		return -1, 0, fmt.Errorf("invalid page: %w", err)
	} else if page <= 0 {
		return -1, 0, errors.New("page must be positive")
	}

	countStr := r.FormValue("count")
	if countStr == "" {
		return page, defaultCount, nil
	}
	count, err = strconv.Atoi(countStr)
	if err != nil {
		return -1, 0, fmt.Errorf("invalid count: %w", err)
	} else if count < 0 {
		return -1, 0, errors.New("count must be non-negative")
	}

	if count == 0 {
		return page, defaultCount, nil
	}
	return page, count, nil
}

// slicePage returns the subslice of vs corresponding to the page and count
// parameters (as returned by parsePageOptions), or nil if the page and count
// are past the end of vs.
func slicePage[T any, S ~[]T](vs S, page, count int) S {
	if page < 0 {
		return vs // take the whole input, no pagination
	}
	start := (page - 1) * count
	end := start + count
	if start >= len(vs) {
		return nil // the page starts after the end of vs
	}
	if end > len(vs) {
		end = len(vs)
	}
	return vs[start:end]
}

func formatEtag(h hash.Hash) string { return fmt.Sprintf(`"%x"`, h.Sum(nil)) }

// newHashPipe returns a reader that delegates to r, and as a side-effect
// writes everything successfully read from r as writes to h.
func newHashPipe(r io.Reader, h hash.Hash) io.Reader { return hashPipe{r: r, h: h} }

type hashPipe struct {
	r io.Reader
	h hash.Hash
}

func (h hashPipe) Read(data []byte) (int, error) {
	nr, err := h.r.Read(data)
	h.h.Write(data[:nr])
	return nr, err
}

// makeFileEtag returns a quoted Etag hash ("<hex>") for the specified file
// path.
func makeFileEtag(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	etagHash := sha256.New()
	if _, err := io.Copy(etagHash, f); err != nil {
		return "", err
	}
	return formatEtag(etagHash), nil
}

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
