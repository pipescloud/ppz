package cli

import "testing"

func TestParseTermSize(t *testing.T) {
	cases := []struct {
		in         string
		cols, rows int
		ok         bool
	}{
		{"120x40", 120, 40, true},
		{"80X24", 80, 24, true},
		{"1000x1000", 1000, 1000, true},
		{"120", 0, 0, false},
		{"0x40", 0, 0, false},
		{"120x-1", 0, 0, false},
		{"1001x40", 0, 0, false},
		{"axb", 0, 0, false},
		{"120x40x2", 0, 0, false},
	}
	for _, c := range cases {
		cols, rows, ok := parseTermSize(c.in)
		if ok != c.ok || cols != c.cols || rows != c.rows {
			t.Errorf("parseTermSize(%q) = %d,%d,%v; want %d,%d,%v", c.in, cols, rows, ok, c.cols, c.rows, c.ok)
		}
	}
}
