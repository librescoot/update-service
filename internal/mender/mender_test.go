package mender

import (
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestManager_RemoveFile(t *testing.T) {
	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "mender_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	logger := log.New(os.Stdout, "test: ", 0)
	manager := NewManager(tmpDir, func() Budget { return Budget{} }, logger)

	// Create a file to remove
	testFile := filepath.Join(tmpDir, "test.mender")
	if err := os.WriteFile(testFile, []byte("test content"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// Remove the file
	err = manager.RemoveFile(testFile)
	if err != nil {
		t.Errorf("RemoveFile failed: %v", err)
	}

	// Verify file is gone
	if _, err := os.Stat(testFile); !os.IsNotExist(err) {
		t.Errorf("File should have been removed")
	}
}

func TestManager_RemoveFile_RefusesOutsideDir(t *testing.T) {
	// Create temp directories
	tmpDir, err := os.MkdirTemp("", "mender_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	otherDir, err := os.MkdirTemp("", "other_test")
	if err != nil {
		t.Fatalf("Failed to create other dir: %v", err)
	}
	defer os.RemoveAll(otherDir)

	logger := log.New(os.Stdout, "test: ", 0)
	manager := NewManager(tmpDir, func() Budget { return Budget{} }, logger)

	// Create a file outside the download directory
	outsideFile := filepath.Join(otherDir, "outside.mender")
	if err := os.WriteFile(outsideFile, []byte("test content"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// Attempt to remove file outside download directory should fail
	err = manager.RemoveFile(outsideFile)
	if err == nil {
		t.Errorf("RemoveFile should have refused to remove file outside download directory")
	}

	// Verify file still exists
	if _, err := os.Stat(outsideFile); os.IsNotExist(err) {
		t.Errorf("File outside download directory should not have been removed")
	}
}

func TestManager_FindMenderFileForVersion(t *testing.T) {
	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "mender_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	logger := log.New(os.Stdout, "test: ", 0)
	manager := NewManager(tmpDir, func() Budget { return Budget{} }, logger)

	// Create a mender file with version in name
	testFile := filepath.Join(tmpDir, "librescoot-unu-dbc-nightly-20251212T024719.mender")
	if err := os.WriteFile(testFile, []byte("test content"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// Should find file by version
	path, found := manager.FindMenderFileForVersion("nightly-20251212T024719")
	if !found {
		t.Errorf("Should have found mender file for version")
	}
	if path != testFile {
		t.Errorf("Expected %s, got %s", testFile, path)
	}

	// Should find with lowercase version
	_, found = manager.FindMenderFileForVersion("nightly-20251212t024719")
	if !found {
		t.Errorf("Should have found mender file for lowercase version")
	}

	// Should not find non-existent version
	_, found = manager.FindMenderFileForVersion("nightly-99999999T999999")
	if found {
		t.Errorf("Should not have found mender file for non-existent version")
	}
}

// runMenderCleanup lays out a download directory, runs the cleanup against the
// given running version, and reports which files survived.
func runMenderCleanup(t *testing.T, runningVersion string, files ...string) map[string]bool {
	t.Helper()

	tmpDir, err := os.MkdirTemp("", "mender_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpDir) })

	logger := log.New(os.Stdout, "test: ", 0)
	manager := NewManager(tmpDir, func() Budget { return Budget{} }, logger)

	for _, f := range files {
		if err := os.WriteFile(filepath.Join(tmpDir, f), []byte("x"), 0644); err != nil {
			t.Fatalf("Failed to create %s: %v", f, err)
		}
	}

	manager.cleanupStaleMenderFiles(runningVersion)

	survived := make(map[string]bool, len(files))
	for _, f := range files {
		_, err := os.Stat(filepath.Join(tmpDir, f))
		switch {
		case err == nil:
			survived[f] = true
		case os.IsNotExist(err):
			survived[f] = false
		default:
			t.Fatalf("Failed to stat %s: %v", f, err)
		}
	}
	return survived
}

// checkKept asserts that exactly the named file survived.
func checkKept(t *testing.T, survived map[string]bool, want ...string) {
	t.Helper()
	wanted := make(map[string]bool, len(want))
	for _, w := range want {
		wanted[w] = true
	}
	for f, ok := range survived {
		if wanted[f] && !ok {
			t.Errorf("expected %s to be kept, it was removed", f)
		}
		if !wanted[f] && ok {
			t.Errorf("expected %s to be removed, it survived", f)
		}
	}
}

// Observed on hardware 2026-08-26: an MDB running a nightly, with a leftover
// stable artifact in /data/ota/mdb, had the nightly artifact reaped right
// after commit and the stable one kept. Cross-channel version.Compare only
// offers a lexicographic tiebreak, and 'v' > 'n'. The kept artifact is the
// delta base, so the next OTA fell back to a full download.
func TestManager_CleanupStaleMenderFiles_KeepsRunningArtifact(t *testing.T) {
	survived := runMenderCleanup(t, "nightly-20260826T021727",
		"librescoot-unu-mdb-v1.2.1.mender",
		"librescoot-unu-mdb-nightly-20260826T021727.mender")

	checkKept(t, survived, "librescoot-unu-mdb-nightly-20260826T021727.mender")
}

// A newer artifact on the running channel is a target that was downloaded but
// not installed, left by a power cut or a failed install. It is kept alongside
// the base: Downloader.Download skips the transfer when the complete file is
// already there, so keeping it makes the retry free, and once installed it
// becomes the next base.
//
// This inverts an earlier expectation that the newer file was reaped as a mere
// "staged download". Reaping it cost a full re-download for no gain.
func TestManager_CleanupStaleMenderFiles_KeepsRunningAndStagedNewer(t *testing.T) {
	survived := runMenderCleanup(t, "v1.2.1",
		"librescoot-unu-mdb-v1.2.1.mender",
		"librescoot-unu-mdb-v1.3.0.mender")

	checkKept(t, survived,
		"librescoot-unu-mdb-v1.2.1.mender",
		"librescoot-unu-mdb-v1.3.0.mender")
}

// Only the newest of several newer ones earns the staged slot. Intermediates
// never need to be on disk: ApplyDeltaChain takes one base plus N deltas and
// produces the final image in flight.
func TestManager_CleanupStaleMenderFiles_KeepsOnlyNewestStaged(t *testing.T) {
	survived := runMenderCleanup(t, "nightly-20260801T000000",
		"librescoot-unu-mdb-nightly-20260801T000000.mender",
		"librescoot-unu-mdb-nightly-20260802T000000.mender",
		"librescoot-unu-mdb-nightly-20260803T000000.mender")

	checkKept(t, survived,
		"librescoot-unu-mdb-nightly-20260801T000000.mender",
		"librescoot-unu-mdb-nightly-20260803T000000.mender")
}

// Older than running is unreachable: there is no backwards delta, and a
// rollback is a mender partition operation that needs no artifact.
func TestManager_CleanupStaleMenderFiles_ReapsOlderThanRunning(t *testing.T) {
	survived := runMenderCleanup(t, "v1.2.1",
		"librescoot-unu-mdb-v1.1.0.mender",
		"librescoot-unu-mdb-v1.2.1.mender")

	checkKept(t, survived, "librescoot-unu-mdb-v1.2.1.mender")
}

// A newer artifact on another channel is neither a valid base (deltas are
// published per channel) nor a plausible target, so the staged slot must not
// take it. v9.9.9 also outranks any nightly on a lexicographic compare, so this
// covers that trap too.
func TestManager_CleanupStaleMenderFiles_ReapsNewerOnOtherChannel(t *testing.T) {
	survived := runMenderCleanup(t, "nightly-20260801T000000",
		"librescoot-unu-mdb-nightly-20260801T000000.mender",
		"librescoot-unu-mdb-v9.9.9.mender")

	checkKept(t, survived, "librescoot-unu-mdb-nightly-20260801T000000.mender")
}

// Filenames carry the timestamp token verbatim while Redis and the release
// tags carry it lowercased, so the anchor match has to ignore case.
func TestManager_CleanupStaleMenderFiles_RunningVersionCaseInsensitive(t *testing.T) {
	survived := runMenderCleanup(t, "nightly-20260826t021727",
		"librescoot-unu-mdb-v1.2.1.mender",
		"librescoot-unu-mdb-nightly-20260826T021727.mender")

	checkKept(t, survived, "librescoot-unu-mdb-nightly-20260826T021727.mender")
}

// Running artifact absent (installed from elsewhere, or already reaped): keep
// the newest on the running channel so a base survives, and do not let the
// other channel win on lexicographic order.
func TestManager_CleanupStaleMenderFiles_FallsBackToRunningChannel(t *testing.T) {
	survived := runMenderCleanup(t, "nightly-20260826T021727",
		"librescoot-unu-mdb-v1.2.1.mender",
		"librescoot-unu-mdb-nightly-20260101T000000.mender",
		"librescoot-unu-mdb-nightly-20260601T000000.mender")

	checkKept(t, survived, "librescoot-unu-mdb-nightly-20260601T000000.mender")
}

// Nothing on the running channel at all: keep the newest of what is there
// rather than emptying the directory, which would leave no base for the
// pending install to build on.
func TestManager_CleanupStaleMenderFiles_RunningChannelAbsent(t *testing.T) {
	survived := runMenderCleanup(t, "v1.2.1",
		"librescoot-unu-mdb-nightly-20260101T000000.mender",
		"librescoot-unu-mdb-nightly-20260601T000000.mender")

	checkKept(t, survived, "librescoot-unu-mdb-nightly-20260601T000000.mender")
}

// mender could not be asked, so ranking is all that is left. v0.10.0 is the
// newest and lex compare would (wrongly) keep v0.7.0. Real published stable
// assets carry no "stable-" infix.
func TestManager_CleanupStaleMenderFiles_UnknownRunningVersionIsSemverAware(t *testing.T) {
	survived := runMenderCleanup(t, "",
		"librescoot-unu-mdb-v0.7.0.mender",
		"librescoot-unu-mdb-v0.8.0.mender",
		"librescoot-unu-mdb-v0.10.0.mender")

	checkKept(t, survived, "librescoot-unu-mdb-v0.10.0.mender")
}

// Real published stable assets are "librescoot-unu-mdb-v1.0.0.mender" — no
// "stable-" infix. FindLatestMenderFile must classify them as the stable
// channel and pick the newest by semver, even when nightly files share the
// directory (e.g. after a channel switch).
func TestManager_FindLatestMenderFile_MixedChannels(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "mender_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	logger := log.New(os.Stdout, "test: ", 0)
	manager := NewManager(tmpDir, func() Budget { return Budget{} }, logger)

	files := []string{
		"librescoot-unu-mdb-v0.9.0.mender",
		"librescoot-unu-mdb-v0.10.0.mender",
		"librescoot-unu-mdb-v1.0.0.mender",
		"librescoot-unu-mdb-nightly-20260101T120000.mender",
		"librescoot-unu-mdb-nightly-20260501T210243.mender",
		"librescoot-unu-mdb-testing-20260501T211040.mender",
	}
	for _, f := range files {
		p := filepath.Join(tmpDir, f)
		if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
			t.Fatalf("Failed to create %s: %v", f, err)
		}
	}

	cases := []struct {
		channel     string
		wantPath    string
		wantVersion string
	}{
		{"stable", filepath.Join(tmpDir, "librescoot-unu-mdb-v1.0.0.mender"), "v1.0.0"},
		{"nightly", filepath.Join(tmpDir, "librescoot-unu-mdb-nightly-20260501T210243.mender"), "nightly-20260501T210243"},
		{"testing", filepath.Join(tmpDir, "librescoot-unu-mdb-testing-20260501T211040.mender"), "testing-20260501T211040"},
	}
	for _, c := range cases {
		path, version, found := manager.FindLatestMenderFile(c.channel)
		if !found {
			t.Errorf("channel=%s: expected to find a file, got found=false", c.channel)
			continue
		}
		if path != c.wantPath {
			t.Errorf("channel=%s: path = %q, want %q", c.channel, path, c.wantPath)
		}
		if version != c.wantVersion {
			t.Errorf("channel=%s: version = %q, want %q", c.channel, version, c.wantVersion)
		}
	}
}

func TestManager_FindLatestMenderFile_NoMatch(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "mender_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	logger := log.New(os.Stdout, "test: ", 0)
	manager := NewManager(tmpDir, func() Budget { return Budget{} }, logger)

	if err := os.WriteFile(filepath.Join(tmpDir, "librescoot-unu-mdb-nightly-20260501T210243.mender"), []byte("x"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	if _, _, found := manager.FindLatestMenderFile("stable"); found {
		t.Errorf("expected no stable file in nightly-only directory")
	}
	if _, _, found := manager.FindLatestMenderFile("testing"); found {
		t.Errorf("expected no testing file in nightly-only directory")
	}
}

func TestManager_CleanupStaleDeltaFiles(t *testing.T) {
	const maxAge = 30 * 24 * time.Hour
	freshMtime := time.Now()
	oldMtime := time.Now().Add(-31 * 24 * time.Hour)
	futureMtime := time.Now().Add(48 * time.Hour)

	// Each spec'd file: name, mtime, and whether it must survive the call.
	type fileSpec struct {
		name  string
		mtime time.Time
		keep  bool
	}

	cases := []struct {
		name  string
		files []fileSpec
	}{
		// Same-channel version reap (fresh mtime, so age never fires).
		{
			name: "same-channel target older than ref",
			files: []fileSpec{
				{"librescoot-unu-dbc-nightly-20260428T013225.mender", freshMtime, true},
				{"librescoot-unu-dbc-nightly-20260101T000000.delta", freshMtime, false},
			},
		},
		{
			name: "same-channel target equal to ref",
			files: []fileSpec{
				{"librescoot-unu-dbc-nightly-20260428T013225.mender", freshMtime, true},
				{"librescoot-unu-dbc-nightly-20260428T013225.delta", freshMtime, false},
			},
		},
		{
			name: "same-channel equal-version partial",
			files: []fileSpec{
				{"librescoot-unu-dbc-nightly-20260428T013225.mender", freshMtime, true},
				{"librescoot-unu-dbc-nightly-20260428T013225.delta.tmp", freshMtime, false},
			},
		},
		{
			name: "same-channel forward delta kept for reuse",
			files: []fileSpec{
				{"librescoot-unu-dbc-nightly-20260428T013225.mender", freshMtime, true},
				{"librescoot-unu-dbc-nightly-20260601T000000.delta", freshMtime, true},
			},
		},
		// Cross-channel: version path must not fire; fresh stays, old reaps via age.
		{
			name: "cross-channel stable delta vs nightly ref fresh",
			files: []fileSpec{
				{"librescoot-unu-dbc-nightly-20260428T013225.mender", freshMtime, true},
				{"librescoot-unu-dbc-v0.8.0.delta", freshMtime, true},
			},
		},
		{
			name: "cross-channel testing delta vs nightly ref fresh",
			files: []fileSpec{
				{"librescoot-unu-dbc-nightly-20260428T013225.mender", freshMtime, true},
				{"librescoot-unu-dbc-testing-20260428T013225.delta", freshMtime, true},
			},
		},
		{
			name: "cross-channel stable delta vs testing ref fresh",
			files: []fileSpec{
				{"librescoot-unu-dbc-testing-20260428T013225.mender", freshMtime, true},
				{"librescoot-unu-dbc-v0.8.0.delta", freshMtime, true},
			},
		},
		{
			name: "cross-channel stable delta vs nightly ref old reaps via age",
			files: []fileSpec{
				{"librescoot-unu-dbc-nightly-20260428T013225.mender", freshMtime, true},
				{"librescoot-unu-dbc-v0.8.0.delta", oldMtime, false},
			},
		},
		// Empty / malformed token.
		{
			name: "malformed token fresh kept",
			files: []fileSpec{
				{"librescoot-unu-dbc-nightly-20260428T013225.mender", freshMtime, true},
				{"garbage.delta", freshMtime, true},
			},
		},
		{
			name: "malformed token old reaps via age",
			files: []fileSpec{
				{"librescoot-unu-dbc-nightly-20260428T013225.mender", freshMtime, true},
				{"garbage.delta", oldMtime, false},
			},
		},
		// No-reference (age-only) mode.
		{
			name: "no reference fresh kept",
			files: []fileSpec{
				{"librescoot-unu-dbc-nightly-20260101T000000.delta", freshMtime, true},
			},
		},
		{
			name: "no reference old reaps via age",
			files: []fileSpec{
				{"librescoot-unu-dbc-nightly-20260101T000000.delta", oldMtime, false},
			},
		},
		// Future-mtime guard: a future mtime must never age-reap.
		{
			name: "no reference future mtime kept",
			files: []fileSpec{
				{"librescoot-unu-dbc-nightly-20260101T000000.delta", futureMtime, true},
			},
		},
		// Multiple .mender files: the reference must be the NEWEST by version,
		// not the first/last/oldest. With newest-wins the delta is <= ref on the
		// same channel and reaps; an oldest-wins reference (20260101) would judge
		// the delta as a forward delta (dv > ref) and wrongly keep it.
		{
			name: "newest of two mender files is the reference",
			files: []fileSpec{
				{"librescoot-unu-dbc-nightly-20260101T000000.mender", freshMtime, true},
				{"librescoot-unu-dbc-nightly-20260601T000000.mender", freshMtime, true},
				{"librescoot-unu-dbc-nightly-20260301T000000.delta", freshMtime, false},
			},
		},
		// Channel-switch-stale-base path (the case the doc comment calls out):
		// the local base .mender is stale, and a same-channel delta is NEWER than
		// it (dv > ref), so the version test declines to reap. Reaping must then
		// fall through to the age backstop.
		{
			name: "stale base mender same-channel newer delta old reaps via age",
			files: []fileSpec{
				{"librescoot-unu-dbc-nightly-20260101T000000.mender", freshMtime, true},
				{"librescoot-unu-dbc-nightly-20260601T000000.delta", oldMtime, false},
			},
		},
		{
			name: "stale base mender same-channel newer delta fresh kept",
			files: []fileSpec{
				{"librescoot-unu-dbc-nightly-20260101T000000.mender", freshMtime, true},
				{"librescoot-unu-dbc-nightly-20260601T000000.delta", freshMtime, true},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tmpDir, err := os.MkdirTemp("", "mender_delta_test")
			if err != nil {
				t.Fatalf("Failed to create temp dir: %v", err)
			}
			defer os.RemoveAll(tmpDir)

			logger := log.New(os.Stdout, "test: ", 0)
			manager := NewManager(tmpDir, func() Budget { return Budget{} }, logger)

			for _, f := range c.files {
				p := filepath.Join(tmpDir, f.name)
				if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
					t.Fatalf("Failed to create %s: %v", f.name, err)
				}
				if err := os.Chtimes(p, f.mtime, f.mtime); err != nil {
					t.Fatalf("Failed to set mtime on %s: %v", f.name, err)
				}
			}

			manager.CleanupStaleDeltaFiles(maxAge)

			for _, f := range c.files {
				p := filepath.Join(tmpDir, f.name)
				_, err := os.Stat(p)
				if f.keep && err != nil {
					t.Errorf("expected %s to survive, got err=%v", f.name, err)
				}
				if !f.keep && !os.IsNotExist(err) {
					t.Errorf("expected %s to be reaped, got err=%v", f.name, err)
				}
			}
		})
	}
}

// When the wall clock has not advanced past deltaSanityEpoch (unsynced boot
// clock), the age backstop must not reap anything, even an ancient mtime that
// would otherwise exceed maxAge. The version path is unaffected; only the age
// branch is gated by the epoch.
func TestManager_CleanupStaleDeltaFiles_UnsyncedClock(t *testing.T) {
	const maxAge = 30 * 24 * time.Hour

	// Push the epoch into the future so now.After(deltaSanityEpoch) is false,
	// simulating a boot clock that has not yet synced.
	saved := deltaSanityEpoch
	deltaSanityEpoch = time.Now().Add(365 * 24 * time.Hour)
	defer func() { deltaSanityEpoch = saved }()

	tmpDir, err := os.MkdirTemp("", "mender_delta_epoch_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	logger := log.New(os.Stdout, "test: ", 0)
	manager := NewManager(tmpDir, func() Budget { return Budget{} }, logger)

	// No reference .mender, so only the age path could reap. With the epoch in
	// the future, the age guard suppresses it and the ancient delta survives.
	ancient := filepath.Join(tmpDir, "librescoot-unu-dbc-nightly-20260101T000000.delta")
	if err := os.WriteFile(ancient, []byte("x"), 0644); err != nil {
		t.Fatalf("Failed to create delta: %v", err)
	}
	oldMtime := time.Now().Add(-365 * 24 * time.Hour)
	if err := os.Chtimes(ancient, oldMtime, oldMtime); err != nil {
		t.Fatalf("Failed to set mtime: %v", err)
	}

	manager.CleanupStaleDeltaFiles(maxAge)

	if _, err := os.Stat(ancient); err != nil {
		t.Errorf("expected ancient delta to survive while clock is unsynced, got err=%v", err)
	}
}

func TestManager_withinDownloadDir(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "mender_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	logger := log.New(os.Stdout, "test: ", 0)
	manager := NewManager(tmpDir, func() Budget { return Budget{} }, logger)

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"file directly inside", filepath.Join(tmpDir, "a.mender"), true},
		{"file in subdirectory", filepath.Join(tmpDir, "sub", "a.mender"), true},
		{"the directory itself", tmpDir, true},
		{"parent directory", filepath.Dir(tmpDir), false},
		{"unrelated absolute path", "/etc/passwd", false},

		{"sibling sharing the name prefix", tmpDir + "-old", false},
		{"traversal escaping the directory", filepath.Join(tmpDir, "..", "..", "etc"), false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := manager.withinDownloadDir(c.path); got != c.want {
				t.Errorf("withinDownloadDir(%q) = %v, want %v", c.path, got, c.want)
			}
		})
	}
}
