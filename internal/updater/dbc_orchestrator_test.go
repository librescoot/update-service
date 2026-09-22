package updater

import (
	"testing"
	"time"

	"github.com/librescoot/update-service/internal/status"
)

// A DBC release with an image for the dashboard variant.
func dbcRelease(tag, publishedAt string, size int64) Release {
	published, err := time.Parse(time.RFC3339, publishedAt)
	if err != nil {
		panic(err)
	}
	return Release{
		TagName:     tag,
		PublishedAt: published,
		Prerelease:  true,
		Assets: []Asset{
			{Name: "librescoot-unu-dbc-" + tag + ".mender", Size: size, URL: "http://example/dbc.mender"},
		},
	}
}

// The DBC is judged against its own channel: the MDB's own list says nothing
// about a channel the MDB is not on.
func TestPreflightDBCUpdateUsesTheDBCChannel(t *testing.T) {
	index := map[string][]Release{
		"nightly": {dbcRelease("nightly-20260921T003447", "2026-09-21T00:45:55Z", 2)},
		"testing": {dbcRelease("testing-20260920T211120", "2026-09-20T21:23:44Z", 1)},
	}
	u, mr := newTestUpdaterForPreview(t, index)
	mr.HSet("settings", "updates.dbc.channel", "testing")
	mr.HSet("version:dbc", "variant_id", "unu-dbc")
	mr.HSet("version:dbc", "version_id", "testing-20260919T000000")

	got := u.preflightDBCUpdate(latestManifest(index))

	if got.result != status.DBCPreflightAvailable || got.version != "testing-20260920T211120" {
		t.Fatalf("preflight = %#v, want the testing release available", got)
	}
	if !got.wake {
		t.Error("a DBC update must wake the dashboard")
	}
}

func TestPreflightDBCUpdate(t *testing.T) {
	tests := []struct {
		name        string
		variant     string
		version     string
		releases    map[string][]Release
		wantResult  string
		wantVersion string
		wantWake    bool
	}{
		{
			name:        "newer release is available",
			variant:     "unu-dbc",
			version:     "v1.2.0",
			releases:    stableIndex(),
			wantResult:  status.DBCPreflightAvailable,
			wantVersion: "v1.3.0",
			wantWake:    true,
		},
		{
			name:        "cached version is current",
			variant:     "unu-dbc",
			version:     "v1.3.0",
			releases:    stableIndex(),
			wantResult:  status.DBCPreflightUpToDate,
			wantVersion: "v1.3.0",
		},
		{
			name:        "known release but missing DBC version is unknown",
			variant:     "unu-dbc",
			releases:    stableIndex(),
			wantResult:  status.DBCPreflightUnknown,
			wantVersion: "v1.3.0",
			wantWake:    true,
		},
		{
			name:       "missing variant must not claim no update",
			variant:    "dbc",
			releases:   stableIndex(),
			wantResult: status.DBCPreflightUnknown,
			wantWake:   true,
		},
		{
			name:       "empty release index has no DBC release",
			variant:    "unu-dbc",
			releases:   map[string][]Release{"stable": {}},
			wantResult: status.DBCPreflightNoRelease,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, mr := newTestUpdaterForPreview(t, tt.releases)
			mr.HSet("settings", "updates.dbc.channel", "stable")
			mr.HSet("version:dbc", "variant_id", tt.variant)
			if tt.version != "" {
				mr.HSet("version:dbc", "version_id", tt.version)
			}

			got := u.preflightDBCUpdate(latestManifest(tt.releases))
			if got.result != tt.wantResult || got.version != tt.wantVersion || got.wake != tt.wantWake {
				t.Fatalf("preflight = %#v, want result=%q version=%q wake=%v", got, tt.wantResult, tt.wantVersion, tt.wantWake)
			}
		})
	}
}
