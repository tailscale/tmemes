// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package tmemes defines a meme generator, putting the meme in TS.
//
// This package defines shared data types used throughout the service.
package tmemes

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"tailscale.com/tailcfg"
)

// A Template defines a base template for an image macro.
type Template struct {
	ID        int            `json:"id"`     // assigned by the server
	Path      string         `json:"path"`   // path of image file
	Width     int            `json:"width"`  // image width
	Height    int            `json:"height"` // image height
	Name      string         `json:"name"`   // descriptive label
	Creator   tailcfg.UserID `json:"creator"`
	CreatedAt time.Time      `json:"createdAt"`
	Areas     []Area         `json:"areas,omitempty"` // optional predefined areas
	Hidden    bool           `json:"hidden,omitempty"`

	// If a template is hidden, macros based on it are still usable, but the
	// service won't list it as available and won't let you create new macros
	// from it. This way we can "delete" a template without screwing up the
	// previous macros that used it.
	//
	// To truly obliterate a template, delete the macros that reference it.
}

// A Macro combines a Template with some text. Macros can be cached by their
// ID, or re-rendered on-demand.
type Macro struct {
	ID          int            `json:"id"`
	TemplateID  int            `json:"templateID"`
	Creator     tailcfg.UserID `json:"creator,omitempty"` // -1 for anon
	CreatedAt   time.Time      `json:"createdAt"`
	TextOverlay []TextLine     `json:"textOverlay"`
	ContextLink []ContextLink  `json:"contextLink,omitempty"`

	Upvotes   int `json:"upvotes,omitempty"`
	Downvotes int `json:"downvotes,omitempty"`
}

// ValidForCreate reports whether m is valid for the creation of a new macro.
func (m *Macro) ValidForCreate() error {
	const maxContextLinks = 3

	switch {
	case m.ID != 0:
		return errors.New("macro ID must be zero")
	case m.TemplateID <= 0:
		return errors.New("macro must have a template ID")
	case len(m.TextOverlay) == 0:
		return errors.New("macro must have an overlay")
	case m.Upvotes != 0 || m.Downvotes != 0:
		return errors.New("macro must not contain votes")
	case m.Creator > 0:
		return errors.New("invalid macro creator")
	case len(m.ContextLink) > maxContextLinks:
		return errors.New("too many context links")
	}

	// Check and sanitize context links: Remove leading and trailing whitespace,
	// verify that the link is a syntactically valid "http" or "https" URL, and
	// then render it properly escaped.
	for i, cl := range m.ContextLink {
		u, err := url.Parse(strings.TrimSpace(cl.URL))
		if err != nil {
			return fmt.Errorf("invalid context URL: %w", err)
		} else if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("invalid context link scheme %q", u.Scheme)
		}
		m.ContextLink[i].URL = u.String()
	}
	for _, tl := range m.TextOverlay {
		if err := tl.ValidForCreate(); err != nil {
			return err
		}
	}
	return nil
}

// Areas is a wrapper for a slice of Area values that optionally decodes from
// JSON as either a single Area object or an array of Area values.
// A length-1 Areas encodes as a plain object.
type Areas []Area

func (a Areas) MarshalJSON() ([]byte, error) {
	if len(a) == 1 {
		return json.Marshal(a[0])
	}
	msgs := make([]json.RawMessage, len(a))
	for i, v := range a {
		data, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("area %d: %w", i, err)
		}
		msgs[i] = data
	}
	return json.Marshal(msgs)
}

func (a *Areas) UnmarshalJSON(data []byte) error {
	if len(data) == 0 {
		return errors.New("empty input")
	}
	switch data[0] {
	case '[':
		return json.Unmarshal(data, (*[]Area)(a))
	case '{':
		var single Area
		if err := json.Unmarshal(data, &single); err != nil {
			return err
		}
		*a = Areas{single}
		return nil
	default:
		return errors.New("invalid input")
	}
}

// An Area defines a region of an image where text is placed.  Each area has an
// anchor point, relative to the top-left of the image, and a target width and
// height as fractions of the image size.  Text drawn within an area should be
// scaled so that the resulting box does not exceed those dimensions.
type Area struct {
	X     float64 `json:"x"`               // x offset of anchor as a fraction 0..1 of width
	Y     float64 `json:"y"`               // y offset of anchor as a fraciton 0..1 of height
	Width float64 `json:"width,omitempty"` // width of text box as a fraction of image width

	// If true, adjust the effective coordinates for each frame by interpolating
	// the distance between the given X, Y and the X, Y of the next area in
	// sequence, when rendering multiple frames.
	//
	// This is ignored when rendering on a single-frame template.
	Tween bool `json:"tween,omitempty"`

	// N.B. If width == 0 or height == 0, the full dimension can be used.
}

// ValidForCreate reports whether a is valid for creation of a new macro.
func (a Area) ValidForCreate() error {
	if a.X < 0 || a.X > 1 {
		return fmt.Errorf("x out of range %g", a.X)
	}
	if a.Y < 0 || a.Y > 1 {
		return fmt.Errorf("y out of range %g", a.Y)
	}
	if a.Width < 0 || a.Width > 1 {
		return fmt.Errorf("width out of range %g", a.Width)
	}
	return nil
}

// A TextLine is a single line of text with an optional alignment.
type TextLine struct {
	Text        string `json:"text"`
	Color       Color  `json:"color"`
	StrokeColor Color  `json:"strokeColor"`

	// The location(s) where the text should be drawn, it must be non-empty.
	// For a single-frame image, only the first entry is used.
	//
	// For a multiple-frame image, the locations are applied cyclically to the
	// frames of the image. Each area occupies an equal fraction of the frames,
	// for example if there are 8 frames and 2 areas, each area is mapped to 4
	// frames (Field[0] to frames 0, 1, 2, 3; Field[1] to frames 4, 5, 6, 7).
	Field Areas `json:"field"`

	// The first point in a multi-frame image where this text should be visible,
	// as a fraction (0..1) of the total frames of the image. For example, in an
	// image with 16 frames, 0.25 represents 4 frames.
	//
	// if > 0, do not show the text line before this frame fraction.
	// If = 0, show the text beginning at the first frame.
	Start float64 `json:"start,omitempty"` // 0..1

	// The last point in a multi-frame image where this text should be visible,
	// as a fraction (0..1) of the total frames of the image. For example, in an
	// image with 10 frames, 0.5 represents 5 frames.
	//
	// If > Start, hide the text after this frame fraction.
	// Otherwise, do not hide the text after the start index.
	End float64 `json:"end,omitempty"` // 0..1

	// TODO: size, typeface, linebreaks in long runs
}

// ValidForCreate reports whether t is valid for creation of a macro.
func (t TextLine) ValidForCreate() error {
	switch {
	case t.Text == "":
		return errors.New("text is empty")
	case len(t.Field) == 0:
		return errors.New("no fields specified")
	case t.Start < 0 || t.Start > 1:
		return fmt.Errorf("start out of range %g", t.Start)
	case t.End < 0 || t.End > 1:
		return fmt.Errorf("end out of range %g", t.End)
	}
	for _, f := range t.Field {
		if err := f.ValidForCreate(); err != nil {
			return err
		}
	}
	return nil
}

// ContextLink is a link to explain the context of a macro.
type ContextLink struct {
	URL  string `json:"url"`            // required
	Text string `json:"text,omitempty"` // optional
}

// MustColor constructs a color from a known color name or hex specification
// #xxx or #xxxxxx. It panics if s does not correspond to a valid color.
func MustColor(s string) Color {
	var c Color
	if err := c.UnmarshalText([]byte(s)); err != nil {
		panic("invalid color: " + err.Error())
	}
	return c
}

// A Color represents an RGB color encoded as hex. It supports encoding in JSON
// as a string, allowing "#xxxxxx" or "#xxx" format (the "#" is optional).
type Color [3]float64

func (c Color) R() float64 { return c[0] }
func (c Color) G() float64 { return c[1] }
func (c Color) B() float64 { return c[2] }

func (c Color) MarshalText() ([]byte, error) {
	s := fmt.Sprintf("#%02x%02x%02x",
		byte(c[0]*255), byte(c[1]*255), byte(c[2]*255))

	// Check for a name mapping.
	if n, ok := c2n[s]; ok {
		s = n
	}
	return []byte(s), nil
}

func (c *Color) UnmarshalText(data []byte) error {
	// As a special case, treat an empty string as "white".
	if len(data) == 0 {
		c[0], c[1], c[2] = 1, 1, 1
		return nil
	}
	p := string(data)

	// Check for a name mapping.
	if c, ok := n2c[p]; ok {
		p = c
	}

	p = strings.TrimPrefix(p, "#")
	var r, g, b byte
	var err error
	switch len(p) {
	case 3:
		_, err = fmt.Sscanf(p, "%1x%1x%1x", &r, &g, &b)
		r |= r << 4
		g |= g << 4
		b |= b << 4
	case 6:
		_, err = fmt.Sscanf(p, "%2x%2x%2x", &r, &g, &b)
	default:
		return errors.New("invalid hex color")
	}
	if err != nil {
		return err
	}
	c[0], c[1], c[2] = float64(r)/255, float64(g)/255, float64(b)/255
	return nil
}

// n2c maps color names to their equivalent hex strings in standard web RGB
// format (#xxxxxx). Names should be normalized to lower-case. If multiple
// names map to the same hex, the reverse mapping will not be deterministic.
var n2c = map[string]string{
	"white":   "#ffffff",
	"silver":  "#c0c0c0",
	"gray":    "#808080",
	"black":   "#000000",
	"red":     "#ff0000",
	"maroon":  "#800000",
	"yellow":  "#ffff00",
	"olive":   "#808000",
	"lime":    "#00ff00",
	"green":   "#008000",
	"aqua":    "#00ffff",
	"teal":    "#008080",
	"blue":    "#0000ff",
	"navy":    "#000080",
	"fuchsia": "#ff00ff",
	"purple":  "#800080",
}

var c2n = make(map[string]string)

func init() {
	// Set up the reverse mapping from color code to name.
	for n, c := range n2c {
		_, ok := c2n[c]
		if !ok {
			c2n[c] = n
		}
	}
}
