package commitgate

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	return NewStoreAt(
		filepath.Join(dir, "commit-gate-mdb.json"),
		filepath.Join(dir, "commit-gate-quarantine-mdb.json"),
	)
}

func TestMarkerRoundTrip(t *testing.T) {
	store := newStore(t)
	firstSeen := time.Date(2026, 9, 21, 18, 43, 31, 0, time.UTC)
	want := Marker{
		Artifact:          "release-nightly-20260921T003447",
		PendingVersion:    "nightly-20260921t003447",
		BootID:            "8f2c1a",
		FirstSeen:         firstSeen,
		RollbackAttempted: true,
		Probes:            map[string]bool{"uptime": true, "systemd": false},
		Verdict:           VerdictWaiting,
		UpdatedAt:         firstSeen.Add(time.Minute),
	}
	if err := store.Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Artifact != want.Artifact || got.PendingVersion != want.PendingVersion || got.BootID != want.BootID {
		t.Fatalf("identity round trip mismatch: %+v", got)
	}
	if !got.FirstSeen.Equal(firstSeen) {
		t.Errorf("FirstSeen = %v, want %v", got.FirstSeen, firstSeen)
	}
	if !got.RollbackAttempted {
		t.Error("RollbackAttempted did not survive the round trip")
	}
	if !got.Probes["uptime"] || got.Probes["systemd"] {
		t.Errorf("Probes = %v, want the recorded states", got.Probes)
	}
}

func TestMarkerMissingIsNotExist(t *testing.T) {
	store := newStore(t)
	if _, err := store.Load(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Load on an absent marker = %v, want os.ErrNotExist", err)
	}
}

func TestSaveRejectsIncompleteMarker(t *testing.T) {
	store := newStore(t)
	if err := store.Save(Marker{Artifact: "release-v1.4.0"}); err == nil {
		t.Fatal("a marker without a boot ID must be refused")
	}
	if err := store.Save(Marker{BootID: "boot-a"}); err == nil {
		t.Fatal("a marker without an artifact must be refused")
	}
}

func TestMarkerClear(t *testing.T) {
	store := newStore(t)
	if err := store.Save(Marker{Artifact: "release-v1.4.0", BootID: "boot-a"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Clear(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Load after Clear = %v, want os.ErrNotExist", err)
	}
	// Clearing an already-clear marker is the normal case at startup.
	if err := store.Clear(); err != nil {
		t.Fatalf("Clear on an absent marker = %v, want nil", err)
	}
}

func TestQuarantineAddIsIdempotent(t *testing.T) {
	store := newStore(t)
	for i := 0; i < 3; i++ {
		if err := store.AddQuarantine("release-nightly-20260921T003447"); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AddQuarantine("release-nightly-20260922T003447"); err != nil {
		t.Fatal(err)
	}

	for _, artifact := range []string{"release-nightly-20260921T003447", "release-nightly-20260922T003447"} {
		quarantined, err := store.Quarantined(artifact)
		if err != nil {
			t.Fatal(err)
		}
		if !quarantined {
			t.Errorf("%s should be quarantined", artifact)
		}
	}

	quarantined, err := store.Quarantined("release-nightly-20260923T003447")
	if err != nil {
		t.Fatal(err)
	}
	if quarantined {
		t.Error("an artifact that was never rejected must not be quarantined")
	}
}

func TestQuarantineEmptyIsNotQuarantined(t *testing.T) {
	store := newStore(t)
	quarantined, err := store.Quarantined("release-v1.4.0")
	if err != nil {
		t.Fatal(err)
	}
	if quarantined {
		t.Error("an absent quarantine list must not quarantine anything")
	}
	if err := store.AddQuarantine(""); err == nil {
		t.Error("an empty artifact must be refused")
	}
}

func TestPruneQuarantineDropsSupersededEntries(t *testing.T) {
	store := newStore(t)
	for _, artifact := range []string{"release-v1.4.0", "release-v1.5.0", "release-v1.6.0"} {
		if err := store.AddQuarantine(artifact); err != nil {
			t.Fatal(err)
		}
	}

	// The vehicle moved past v1.5.0: only v1.6.0 can still be a target.
	if err := store.PruneQuarantine(func(artifact string) bool {
		return artifact != "release-v1.4.0" && artifact != "release-v1.5.0"
	}); err != nil {
		t.Fatal(err)
	}

	for artifact, want := range map[string]bool{
		"release-v1.4.0": false,
		"release-v1.5.0": false,
		"release-v1.6.0": true,
	} {
		got, err := store.Quarantined(artifact)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("Quarantined(%s) = %v, want %v", artifact, got, want)
		}
	}
}

func TestPruneQuarantineToEmptyRemovesTheList(t *testing.T) {
	store := newStore(t)
	if err := store.AddQuarantine("release-v1.4.0"); err != nil {
		t.Fatal(err)
	}
	if err := store.PruneQuarantine(func(string) bool { return false }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.quarantinePath); !os.IsNotExist(err) {
		t.Fatalf("expected the empty list to be removed, stat err = %v", err)
	}
	quarantined, err := store.Quarantined("release-v1.4.0")
	if err != nil {
		t.Fatal(err)
	}
	if quarantined {
		t.Error("a pruned artifact must not stay quarantined")
	}
}

func TestPruneQuarantineNoChangeLeavesTheFile(t *testing.T) {
	store := newStore(t)
	if err := store.AddQuarantine("release-v1.4.0"); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(store.quarantinePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PruneQuarantine(func(string) bool { return true }); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(store.quarantinePath)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("a prune that keeps everything must not rewrite the list")
	}
}

func TestStorePathsAreComponentScoped(t *testing.T) {
	mdb := NewStore("mdb")
	dbc := NewStore("dbc")
	if mdb.markerPath == dbc.markerPath || mdb.quarantinePath == dbc.quarantinePath {
		t.Fatal("mdb and dbc must not share gate state")
	}
	if mdb.markerPath != "/data/ota/commit-gate-mdb.json" {
		t.Errorf("marker path = %q", mdb.markerPath)
	}
	if mdb.quarantinePath != "/data/ota/commit-gate-quarantine-mdb.json" {
		t.Errorf("quarantine path = %q", mdb.quarantinePath)
	}
}
