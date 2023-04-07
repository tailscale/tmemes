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
	"strings"
	"time"

	"tailscale.com/tailcfg"
)

// A Template defines a base template for an image macro.
type Template struct {
	ID        int            `json:"id"`
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

	Upvotes   int `json:"upvotes,omitempty"`
	Downvotes int `json:"downvotes,omitempty"`
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

// An Area defines a rectangular region of an image where text can go.  Each
// area has an anchor point, relative to the top-left of the image, and a
// target width and height as fractions of the image size.  Text drawn within
// an area should be scaled so that the resulting box does not exceed those
// dimensions.
type Area struct {
	X      float64 `json:"x"`                // x offset of anchor as a fraction 0..1 of width
	Y      float64 `json:"y"`                // y offset of anchor as a fraciton 0..1 of width
	Width  float64 `json:"width,omitempty"`  // width of text box as a fraction of image width
	Height float64 `json:"height,omitempty"` // height of text box as a fraction of image height

	// If true, interpolate the distance between this area and the next in
	// sequence, when rendering multiple frames.
	Tween bool `json:"tween,omitempty"`

	// N.B. If width == 0 or height == 0, the full dimension can be used.
}

// A TextLine is a single line of text with an optional alignment.
type TextLine struct {
	Text        string `json:"text"`
	Field       Areas  `json:"field"`
	Color       Color  `json:"color"`
	StrokeColor Color  `json:"strokeColor"`

	// TODO: size, typeface, linebreaks in long runs
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

// A Color represents an RGB color encoded as hex.
// Allows xxxxxx or xxx format.
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
