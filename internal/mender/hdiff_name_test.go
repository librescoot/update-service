package mender

import "testing"

func TestMenderNameFromDelta(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"librescoot-unu-mdb-nightly-20261105T000000.delta", "librescoot-unu-mdb-nightly-20261105T000000.mender"},
		{"librescoot-unu-mdb-nightly-20261105T000000.hdelta", "librescoot-unu-mdb-nightly-20261105T000000.mender"},
	} {
		if got := menderNameFromDelta(tc.input); got != tc.want {
			t.Errorf("%q => %q, want %q", tc.input, got, tc.want)
		}
	}
}
