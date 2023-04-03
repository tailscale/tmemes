package tmemes

import "testing"

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
