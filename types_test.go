// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package tmemes

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestColorNames(t *testing.T) {
	for n, c := range n2c {
		var in1 Color
		if err := in1.UnmarshalText([]byte(n)); err != nil {
			t.Errorf("Unmarshal %q: unexpected error: %v", n, err)
			continue
		}
		var in2 Color
		if err := in2.UnmarshalText([]byte(c)); err != nil {
			t.Errorf("Unmarshal %q: unexpected error: %v", c, err)
			continue
		}

		if in1 != in2 {
			t.Errorf("Colors differ: %v ≠ %v", in1, in2)
		}

		out, err := in1.MarshalText()
		if err != nil {
			t.Errorf("Marshal %v: unexpected error: %v", in1, err)
			continue
		}
		if got := string(out); got != n {
			t.Errorf("Marshal %v: got %q, want %q", in1, got, n)
		}
	}
}

func TestAreas(t *testing.T) {
	tests := []struct {
		input  string
		want   Areas
		output string
	}{
		{"[]", Areas{}, "[]"},
		{"{}", Areas{{}}, `{"x":0,"y":0}`},
		{`{"x":25, "width": -99}`, Areas{{X: 25, Width: -99}}, `{"x":25,"y":0,"width":-99}`},
		{`[
          {"x": 1, "width": 3, "y": 2, "foo":true},
          { "y": 5, "x":4, "height": 6 }
       ]`, Areas{
			{X: 1, Y: 2, Width: 3},
			{X: 4, Y: 5},
		}, `[{"x":1,"y":2,"width":3},{"x":4,"y":5}]`},
	}
	for _, tc := range tests {
		var val Areas
		if err := json.Unmarshal([]byte(tc.input), &val); err != nil {
			t.Fatalf("Unmarshal %q: %v", tc.input, err)
		}
		if diff := cmp.Diff(tc.want, val); diff != "" {
			t.Errorf("Incorrect value (-want, +got):\n%s", diff)
		}
		bits, err := json.Marshal(val)
		if err != nil {
			t.Fatalf("Marshal %v: %v", val, err)
		}
		if got := string(bits); got != tc.output {
			t.Errorf("Marshal: got %#q, want %#q", got, tc.output)
		}
	}
}
