package backoff

import (
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	return NewStore(dir, log.New(os.Stdout, "test: ", 0)), dir
}

func TestStore_NoStateMeansNoSkip(t *testing.T) {
	s, _ := newTestStore(t)
	skip, _ := s.ShouldSkip("v1.0.0", time.Now())
	if skip {
		t.Fatal("a component with no recorded aborts must not be skipped")
	}
}

func TestStore_LadderProgression(t *testing.T) {
	s, _ := newTestStore(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

	want := []time.Duration{time.Hour, 3 * time.Hour, 6 * time.Hour, 24 * time.Hour, 24 * time.Hour}
	for i, expected := range want {
		got, err := s.RecordAbort("v1.0.0", 0, now)
		if err != nil {
			t.Fatalf("abort %d: %v", i+1, err)
		}
		if diff := got.Sub(now); diff != expected {
			t.Errorf("abort %d: retry after %v, want %v", i+1, diff, expected)
		}
	}
}

func TestStore_SkipsUntilRetryAfter(t *testing.T) {
	s, _ := newTestStore(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	if _, err := s.RecordAbort("v1.0.0", 0, now); err != nil {
		t.Fatal(err)
	}

	if skip, _ := s.ShouldSkip("v1.0.0", now.Add(30*time.Minute)); !skip {
		t.Error("must skip inside the backoff window")
	}
	if skip, _ := s.ShouldSkip("v1.0.0", now.Add(2*time.Hour)); skip {
		t.Error("must not skip once the window has passed")
	}
}

func TestStore_NewTargetResetsLadder(t *testing.T) {
	s, _ := newTestStore(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	for range 3 {
		if _, err := s.RecordAbort("v1.0.0", 0, now); err != nil {
			t.Fatal(err)
		}
	}

	if skip, _ := s.ShouldSkip("v2.0.0", now); skip {
		t.Error("a different target must not inherit the old backoff")
	}
	got, err := s.RecordAbort("v2.0.0", 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if diff := got.Sub(now); diff != time.Hour {
		t.Errorf("new target starts at rung 1, got %v", diff)
	}
}

func TestStore_ProgressResetsLadder(t *testing.T) {
	s, _ := newTestStore(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	for range 3 {
		if _, err := s.RecordAbort("v1.0.0", 0, now); err != nil {
			t.Fatal(err)
		}
	}

	retryAfter, err := s.RecordAbort("v1.0.0", ProgressResetBytes, now)
	if err != nil {
		t.Fatal(err)
	}
	if !retryAfter.IsZero() {
		t.Errorf("an attempt that made progress must impose no backoff, got %v", retryAfter)
	}
	if skip, _ := s.ShouldSkip("v1.0.0", now); skip {
		t.Error("must not skip after a productive attempt")
	}
}

func TestStore_ClearRemovesState(t *testing.T) {
	s, dir := newTestStore(t)
	now := time.Now()
	if _, err := s.RecordAbort("v1.0.0", 0, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Clear(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, stateFileName)); !os.IsNotExist(err) {
		t.Errorf("expected state file to be gone, got %v", err)
	}
	if skip, _ := s.ShouldSkip("v1.0.0", now); skip {
		t.Error("cleared state must not skip")
	}
}

func TestStore_ClearOnMissingFileIsNotAnError(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.Clear(); err != nil {
		t.Fatalf("clearing absent state must be a no-op, got %v", err)
	}
}

func TestStore_TornFileFailsOpen(t *testing.T) {
	s, dir := newTestStore(t)
	if err := os.WriteFile(filepath.Join(dir, stateFileName), []byte(`{"target":"v1.0.0","abo`), 0644); err != nil {
		t.Fatal(err)
	}
	if skip, _ := s.ShouldSkip("v1.0.0", time.Now()); skip {
		t.Error("unparseable state must fail open, not block updates")
	}
}

func TestStore_WriteIsAtomic(t *testing.T) {
	s, dir := newTestStore(t)
	if _, err := s.RecordAbort("v1.0.0", 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != stateFileName {
			t.Errorf("temp file %q left behind; write must rename into place", e.Name())
		}
	}
}

func TestStore_IgnoresBackoffWhenClockIsUnset(t *testing.T) {
	s, _ := newTestStore(t)
	real := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	if _, err := s.RecordAbort("v1.0.0", 0, real); err != nil {
		t.Fatal(err)
	}
	// A boot before NTP sync. Honouring retry_after here could park updates
	// forever, so the guard must ignore it.
	unsynced := time.Date(1970, 1, 2, 0, 0, 0, 0, time.UTC)
	if skip, _ := s.ShouldSkip("v1.0.0", unsynced); skip {
		t.Error("backoff must be ignored when the clock is clearly unset")
	}
}
