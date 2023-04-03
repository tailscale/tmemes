// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package tmemes

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
	for n, c := range n2c {
		_, ok := c2n[c]
		if !ok {
			c2n[c] = n
		}
	}
}
