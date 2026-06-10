package version

import "testing"

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		// Stable: FromFilename produces bare semver ("v1.0.0"), no channel prefix.
		// Same channel ("" / ""), so the semver path engages — must NOT lex-compare.
		{"v0.7.0", "v0.10.0", -1},
		{"v0.10.0", "v0.7.0", 1},
		{"v1.0.0", "v0.99.99", 1},
		{"v0.8.0", "v0.8.0", 0},

		// Timestamp tokens: lex compare is correct.
		{"nightly-20260101T120000", "nightly-20260415T120000", -1},
		{"nightly-20260415T120000", "nightly-20260101T120000", 1},

		// Cross-channel: falls back to lex (stable across runs is enough).
		{"nightly-20260101T120000", "v0.7.0", -1},
		{"v0.7.0", "nightly-20260101T120000", 1},
	}
	for _, c := range cases {
		got := Compare(c.a, c.b)
		if got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestFromFilename(t *testing.T) {
	cases := []struct{ in, want string }{
		{"librescoot-unu-mdb-v1.0.0.mender", "v1.0.0"},
		{"librescoot-unu-dbc-nightly-20251226T091616.mender", "nightly-20251226T091616"},
		{"/data/ota/librescoot-unu-mdb-testing-20260101T000000.mender", "testing-20260101T000000"},
		{"too-short.mender", ""},
	}
	for _, c := range cases {
		if got := FromFilename(c.in); got != c.want {
			t.Errorf("FromFilename(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestChannel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"v1.0.0", "stable"},
		{"1.0.0", "stable"},
		{"nightly-20251226T091616", "nightly"},
		{"testing-20251226T091616", "testing"},
		{"garbage", ""},
	}
	for _, c := range cases {
		if got := Channel(c.in); got != c.want {
			t.Errorf("Channel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
