package updater

import (
	"testing"

	"github.com/librescoot/update-service/internal/status"
)

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

			got := u.preflightDBCUpdate(tt.releases["stable"])
			if got.result != tt.wantResult || got.version != tt.wantVersion || got.wake != tt.wantWake {
				t.Fatalf("preflight = %#v, want result=%q version=%q wake=%v", got, tt.wantResult, tt.wantVersion, tt.wantWake)
			}
		})
	}
}
