// Package version compares and extracts LibreScoot firmware version strings.
// Stable versions are bare semver ("v1.0.0"); nightly/testing versions carry a
// "channel-" prefix with an ISO-timestamp token ("nightly-20251226T091616").
package version

import (
	"path/filepath"
	"strconv"
	"strings"
)

// FromFilename extracts the version token from a mender filename.
// Stable filenames are "librescoot-unu-mdb-v1.0.0.mender" -> "v1.0.0".
// Nightly/testing filenames are
// "librescoot-unu-mdb-nightly-20251226T091616.mender" -> "nightly-20251226T091616".
// It also accepts ".delta", ".delta.tmp", and ".mender.tmp" filenames and yields
// the same token as the corresponding ".mender" (a delta's filename embeds its
// target version). Returns "" if no version token can be found.
func FromFilename(filename string) string {
	basename := filepath.Base(filename)
	// Strip exactly one known suffix, longest/compound forms first so
	// ".delta.tmp" matches before ".delta" and we never half-strip.
	for _, suffix := range []string{".mender.tmp", ".delta.tmp", ".mender", ".delta"} {
		if strings.HasSuffix(basename, suffix) {
			basename = strings.TrimSuffix(basename, suffix)
			break
		}
	}
	// The component prefix ends at the 3rd hyphen: librescoot-unu-mdb-
	parts := strings.SplitN(basename, "-", 4)
	if len(parts) < 4 {
		return ""
	}
	return parts[3]
}

// Channel returns the channel implied by a version token as produced by
// FromFilename. Stable versions are bare semver ("v1.0.0"); nightly/testing
// carry a "channel-" prefix. Returns "" if the channel cannot be determined.
func Channel(version string) string {
	if strings.HasPrefix(version, "nightly-") {
		return "nightly"
	}
	if strings.HasPrefix(version, "testing-") {
		return "testing"
	}
	if strings.HasPrefix(version, "v") || (len(version) > 0 && version[0] >= '0' && version[0] <= '9') {
		return "stable"
	}
	return ""
}

// Compare compares two version strings as produced by FromFilename.
//
// When both versions share the same channel and both tokens parse as
// v-prefixed dotted semver, the comparison is semver-aware so that v0.10.0 >
// v0.7.0. Otherwise (different channels, or non-semver tokens like ISO
// timestamps), it falls back to lexicographic, which is correct for
// timestamp-based versions and stable across channels.
//
// Returns -1, 0, or 1 for a<b, a==b, a>b.
func Compare(a, b string) int {
	aChannel, aToken := splitChannelToken(a)
	bChannel, bToken := splitChannelToken(b)
	if aChannel == bChannel {
		if aOK, aParts := parseSemver(aToken); aOK {
			if bOK, bParts := parseSemver(bToken); bOK {
				for i := range 3 {
					if aParts[i] != bParts[i] {
						if aParts[i] < bParts[i] {
							return -1
						}
						return 1
					}
				}
				return 0
			}
		}
	}
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func splitChannelToken(v string) (channel, token string) {
	idx := strings.Index(v, "-")
	if idx < 0 {
		return "", v
	}
	return v[:idx], v[idx+1:]
}

func parseSemver(v string) (bool, [3]int) {
	var out [3]int
	if !strings.HasPrefix(v, "v") {
		return false, out
	}
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) != 3 {
		return false, out
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return false, out
		}
		out[i] = n
	}
	return true, out
}
