package updater

import (
	"strings"
	"testing"
)

// mdbRelease builds a nightly release carrying a .mender for unu-mdb, and a
// .delta only when the build actually produced one.
func mdbRelease(tag string, withDelta bool) Release {
	assets := []Asset{{Name: "librescoot-unu-mdb-" + tag + ".mender", Size: 161762304}}
	if withDelta {
		assets = append(assets, Asset{Name: "librescoot-unu-mdb-" + tag + ".delta", Size: 404540})
	}
	return Release{TagName: tag, Prerelease: true, Assets: assets}
}

func tagsOf(releases []Release) []string {
	tags := make([]string, len(releases))
	for i, r := range releases {
		tags[i] = r.TagName
	}
	return tags
}

func TestBuildDeltaChain(t *testing.T) {
	cases := []struct {
		name     string
		releases []Release
		current  string
		wantTags []string
		wantErr  bool
	}{
		{
			name: "contiguous chain",
			releases: []Release{
				mdbRelease("nightly-20260823T021701", true),
				mdbRelease("nightly-20260823T082958", true),
				mdbRelease("nightly-20260824T133115", true),
			},
			current:  "nightly-20260823t021701",
			wantTags: []string{"nightly-20260823T082958", "nightly-20260824T133115"},
		},
		{
			// The 2026-08-24 incident: a transient CI failure published
			// 20260824T123219 with no delta, and the release after it built
			// its delta against 123219. Stepping over the hole yields a chain
			// whose last link cannot apply.
			name: "release in the middle has no delta",
			releases: []Release{
				mdbRelease("nightly-20260823T021701", true),
				mdbRelease("nightly-20260823T082958", true),
				mdbRelease("nightly-20260824T123219", false),
				mdbRelease("nightly-20260824T133115", true),
			},
			current: "nightly-20260823t021701",
			wantErr: true,
		},
		{
			name: "latest release has no delta",
			releases: []Release{
				mdbRelease("nightly-20260823T082958", true),
				mdbRelease("nightly-20260824T133115", false),
			},
			current: "nightly-20260823t082958",
			wantErr: true,
		},
		{
			// The base only needs a local .mender, never a .delta, so a base
			// whose own build produced no delta must not break the chain.
			name: "base has no delta of its own",
			releases: []Release{
				mdbRelease("nightly-20260823T082958", false),
				mdbRelease("nightly-20260824T133115", true),
			},
			current:  "nightly-20260823t082958",
			wantTags: []string{"nightly-20260824T133115"},
		},
		{
			name: "already at latest",
			releases: []Release{
				mdbRelease("nightly-20260823T082958", true),
				mdbRelease("nightly-20260824T133115", true),
			},
			current:  "nightly-20260824t133115",
			wantTags: nil,
		},
		{
			name:     "base not on the channel",
			releases: []Release{mdbRelease("nightly-20260824T133115", true)},
			current:  "nightly-20260101t000000",
			wantErr:  true,
		},
	}

	u := &Updater{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chain, err := u.buildDeltaChain(tc.releases, tc.current, "nightly", "unu-mdb")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("buildDeltaChain() = %v, want error", tagsOf(chain))
				}
				return
			}
			if err != nil {
				t.Fatalf("buildDeltaChain() error = %v, want chain %v", err, tc.wantTags)
			}
			got := tagsOf(chain)
			if strings.Join(got, ",") != strings.Join(tc.wantTags, ",") {
				t.Errorf("buildDeltaChain() = %v, want %v", got, tc.wantTags)
			}
		})
	}
}
