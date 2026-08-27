package mender

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// newVersionTestManager builds a Manager whose os-release is a fixture. An
// empty body writes no file at all, so the read fails the way a missing
// os-release would.
func newVersionTestManager(t *testing.T, osRelease string) *Manager {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "os-release")
	if osRelease != "" {
		if err := os.WriteFile(path, []byte(osRelease), 0644); err != nil {
			t.Fatalf("writing fixture: %v", err)
		}
	}
	m := NewManager(dir, func() Budget { return Budget{} }, log.New(os.Stdout, "test: ", 0))
	m.osReleasePath = path
	return m
}

func TestOsReleaseVersion(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    string
		wantErr bool
	}{
		{
			name: "quoted value among other keys",
			body: "ID=librescoot-dbc\nVERSION=\"nightly-20260827T152824 (wrynose)\"\nVERSION_ID=nightly-20260827t152824\nVARIANT_ID=unu-dbc\n",
			want: "nightly-20260827t152824",
		},
		{
			name: "double-quoted",
			body: "VERSION_ID=\"v1.3.0\"\n",
			want: "v1.3.0",
		},
		{
			name: "no trailing newline",
			body: "VERSION_ID=v1.2.1",
			want: "v1.2.1",
		},
		{
			// VERSION= must not be mistaken for VERSION_ID=.
			name:    "VERSION present but no VERSION_ID",
			body:    "ID=librescoot-mdb\nVERSION=\"nightly-20260827T152824 (wrynose)\"\n",
			wantErr: true,
		},
		{
			name:    "empty VERSION_ID",
			body:    "VERSION_ID=\n",
			wantErr: true,
		},
		{
			name:    "missing file",
			body:    "",
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := newVersionTestManager(t, tc.body)
			got, err := m.osReleaseVersion()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// os-release is the key the delta base is looked up by, so it is what the
// cleanup must anchor on. mender-update is absent under test, which also
// exercises the path where only os-release can answer.
func TestRunningVersionComesFromOsRelease(t *testing.T) {
	m := newVersionTestManager(t, "VERSION_ID=nightly-20260827t152824\n")
	if got := m.runningVersion(); got != "nightly-20260827t152824" {
		t.Errorf("got %q, want the os-release version", got)
	}
}

// With os-release unreadable the lookup falls back to mender, and with no
// mender either it must yield "", which sends the cleanup to its ranked
// fallbacks rather than to a guess. Skipped where a mender-update exists on
// PATH, because some dev hosts carry a stub of it that answers anything.
func TestRunningVersionEmptyWhenNothingCanAnswer(t *testing.T) {
	if _, err := exec.LookPath("mender-update"); err == nil {
		t.Skip("mender-update on PATH would answer; this covers the case nothing can")
	}
	m := newVersionTestManager(t, "")
	if got := m.runningVersion(); got != "" {
		t.Errorf("got %q, want \"\"", got)
	}
}
