package updater

import (
	"strings"
	"testing"
)

func TestNormalizeDeltaBase(t *testing.T) {
	cases := []struct {
		current, channel, want string
	}{
		{"nightly-20260101T000000", "nightly", "nightly-20260101t000000"},
		{"20260101T000000", "nightly", "nightly-20260101t000000"}, // legacy bare timestamp
		{"testing-20260101T000000", "testing", "testing-20260101t000000"},
		{"v1.2.3", "stable", "v1.2.3"},
		{"1.2.3", "stable", "v1.2.3"},
		{"", "nightly", ""},
	}
	for _, tc := range cases {
		if got := normalizeDeltaBase(tc.current, tc.channel); got != tc.want {
			t.Errorf("normalizeDeltaBase(%q, %q) = %q, want %q", tc.current, tc.channel, got, tc.want)
		}
	}
}

func TestSameVersionInstallErr(t *testing.T) {
	cases := []struct {
		name    string
		target  string // as extracted from the .mender filename
		current string // version_id from redis
		wantErr string // substring; "" means proceed
	}{
		{"same stable, current without v prefix", "v1.3.0", "1.3.0", "already installed"},
		{"same stable, both v-prefixed", "v1.3.0", "v1.3.0", "already installed"},
		{"same stable semver normalization", "v0.10.0", "0.10.0", "already installed"},
		{"same nightly, legacy bare timestamp", "nightly-20260101T000000", "20260101T000000", "already installed"},
		{"newer stable", "v1.4.0", "1.3.0", ""},
		{"older stable (downgrade not gated here)", "v1.2.0", "1.3.0", ""},
		{"same nightly", "nightly-20260102T000000", "nightly-20260102t000000", "already installed"},
		{"cross-channel is not a same-version reinstall", "v1.3.0", "testing-20260101T000000", ""},
		{"unparseable target proceeds", "", "v1.3.0", ""},
		{"unknown installed version proceeds", "v1.3.0", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := sameVersionInstallErr(tc.target, tc.current)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestValidateDeltaTarget(t *testing.T) {
	cases := []struct {
		name     string
		target   string // as extracted from the delta filename
		current  string // version_id from redis
		wantBase string
		wantErr  string // substring; "" means success
	}{
		{
			"nightly next release",
			"nightly-20260102T000000", "nightly-20260101T000000",
			"nightly-20260101t000000", "",
		},
		{
			"legacy bare timestamp current",
			"nightly-20260102T000000", "20260101T000000",
			"nightly-20260101t000000", "",
		},
		{
			"stable semver",
			"v0.10.0", "v0.9.0",
			"v0.9.0", "",
		},
		{
			"stable without v prefix",
			"v0.10.0", "0.9.0",
			"v0.9.0", "",
		},
		{"unparseable target", "", "nightly-20260101T000000", "", "cannot parse"},
		{"unknown installed version", "nightly-20260102T000000", "", "", "installed version unknown"},
		{"channel mismatch", "nightly-20260102T000000", "testing-20260101T000000", "", "channel"},
		{"not newer", "nightly-20260101T000000", "nightly-20260101T000000", "", "not newer"},
		{"older than installed", "nightly-20251231T000000", "nightly-20260101T000000", "", "not newer"},
		{"stable not newer semver-aware", "v0.2.0", "v0.10.0", "", "not newer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, err := validateDeltaTarget(tc.target, tc.current)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if base != tc.wantBase {
					t.Errorf("base = %q, want %q", base, tc.wantBase)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil (base=%q)", tc.wantErr, base)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}
