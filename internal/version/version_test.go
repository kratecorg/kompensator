package version

import "testing"

func TestCompare(t *testing.T) {
	prod142 := Parse("v1.4.2")
	prod150 := Parse("v1.5.0")
	devOld := Parse("dev+2026-06-20T10:00:00Z+aaaaaaa")
	devNew := Parse("dev+2026-06-24T10:00:00Z+bbbbbbb")
	devBare := Parse("dev") // old binary with no embedded time

	cases := []struct {
		name string
		a, b Info
		want int
	}{
		{"release newer than release", prod150, prod142, 1},
		{"release older than release", prod142, prod150, -1},
		{"release equal release", prod142, Parse("v1.4.2"), 0},
		{"release always beats dev", prod142, devNew, 1},
		{"dev never beats release", devNew, prod142, -1},
		{"newer dev beats older dev", devNew, devOld, 1},
		{"older dev loses to newer dev", devOld, devNew, -1},
		{"equal dev", devOld, Parse("dev+2026-06-20T10:00:00Z+aaaaaaa"), 0},
		{"timed dev beats bare dev", devOld, devBare, 1},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("%s: Compare(%s, %s) = %d, want %d", c.name, c.a.Token(), c.b.Token(), got, c.want)
		}
	}
}

func TestParseRoundTrip(t *testing.T) {
	for _, tok := range []string{"v1.4.2", "dev+2026-06-24T10:00:00Z+bbbbbbb"} {
		if got := Parse(tok).Token(); got != tok {
			t.Errorf("round trip %q -> %q", tok, got)
		}
	}
}
