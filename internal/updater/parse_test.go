package updater

import "testing"

func TestParseUpdateSource(t *testing.T) {
	u := &Updater{}
	cases := []struct {
		name         string
		in           string
		wantSrc      string
		wantChecksum string
		wantURL      bool
	}{
		{"url no checksum", "https://h/f.mender", "https://h/f.mender", "", true},
		{"url fragment checksum", "https://h/f.mender#sha256=abc", "https://h/f.mender", "sha256:abc", true},
		{"url legacy checksum", "https://h/f.mender:sha256:abc", "https://h/f.mender", "sha256:abc", true},
		{"http url fragment", "http://h/f.mender#sha256=def", "http://h/f.mender", "sha256:def", true},
		{"file no checksum", "/data/f.mender", "/data/f.mender", "", false},
		{"file fragment checksum", "/data/f.mender#sha256=abc", "/data/f.mender", "sha256:abc", false},
		{"file legacy checksum", "/data/f.mender:sha256:abc", "/data/f.mender", "sha256:abc", false},
		{"file:// scheme", "file:///data/f.mender#sha256=abc", "file:///data/f.mender", "sha256:abc", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, checksum, isURL := u.parseUpdateSource(tc.in)
			if src != tc.wantSrc || checksum != tc.wantChecksum || isURL != tc.wantURL {
				t.Errorf("parseUpdateSource(%q) = (%q, %q, %t), want (%q, %q, %t)",
					tc.in, src, checksum, isURL, tc.wantSrc, tc.wantChecksum, tc.wantURL)
			}
		})
	}
}
