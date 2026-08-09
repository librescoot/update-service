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
	if s.ShouldSkip("v1.0.0") {
		t.Fatal("a component with no recorded aborts must not be skipped")
	}
}

func TestStore_LadderProgressionSixHourInterval(t *testing.T) {
	s, _ := newTestStore(t)
	// Under the 6h default check interval, the 1h/3h/6h rungs all convert to
	// a single skipped check and only the 24h cap converts to more than one.
	want := []int{1, 1, 1, 4, 4}
	for i, expected := range want {
		got, err := s.RecordAbort("v1.0.0", 0, 6*time.Hour)
		if err != nil {
			t.Fatalf("abort %d: %v", i+1, err)
		}
		if got != expected {
			t.Errorf("abort %d: skip_checks = %d, want %d", i+1, got, expected)
		}
	}
}

func TestStore_LadderProgressionThirtyMinuteInterval(t *testing.T) {
	s, _ := newTestStore(t)
	// A shortened check interval must still honour the intended wall-clock
	// delay: each rung converts to more checks, not the same count.
	want := []int{2, 6, 12, 48}
	for i, expected := range want {
		got, err := s.RecordAbort("v1.0.0", 0, 30*time.Minute)
		if err != nil {
			t.Fatalf("abort %d: %v", i+1, err)
		}
		if got != expected {
			t.Errorf("abort %d: skip_checks = %d, want %d", i+1, got, expected)
		}
	}
}

func TestStore_CheckIntervalZeroFallsBackToSixHours(t *testing.T) {
	s, _ := newTestStore(t)
	// checkInterval 0 means automatic checks are disabled; the conversion
	// must still produce a sane count rather than dividing by zero.
	got, err := s.RecordAbort("v1.0.0", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Errorf("skip_checks = %d, want 1 (the 6h fallback conversion of the 1h rung)", got)
	}
}

func TestStore_ShouldSkipDecrementsThenStops(t *testing.T) {
	s, _ := newTestStore(t)
	for i := 0; i < 3; i++ {
		if _, err := s.RecordAbort("v1.0.0", 0, 6*time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	// 4th abort lands on the 24h rung, which converts to 4 skipped checks
	// under a 6h interval.
	got, err := s.RecordAbort("v1.0.0", 0, 6*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got != 4 {
		t.Fatalf("setup: skip_checks = %d, want 4", got)
	}

	for i := 0; i < 4; i++ {
		if !s.ShouldSkip("v1.0.0") {
			t.Fatalf("check %d: expected a skip", i+1)
		}
	}
	if s.ShouldSkip("v1.0.0") {
		t.Error("expected no more skips once the count is exhausted")
	}
}

func TestStore_ShouldSkipPersistsAcrossInstances(t *testing.T) {
	s, dir := newTestStore(t)
	if _, err := s.RecordAbort("v1.0.0", 0, 6*time.Hour); err != nil {
		t.Fatal(err)
	}
	if !s.ShouldSkip("v1.0.0") {
		t.Fatal("expected a skip")
	}
	// A fresh Store pointed at the same file must see the decrement, not the
	// original count: that is what makes the ladder survive a process
	// restart, which is the entire reason state lives on /data.
	s2 := NewStore(dir, log.New(os.Stdout, "test: ", 0))
	if s2.ShouldSkip("v1.0.0") {
		t.Error("expected the single skip_checks to already be exhausted")
	}
}

func TestStore_ProgressResetsLadder(t *testing.T) {
	s, _ := newTestStore(t)
	for i := 0; i < 3; i++ {
		if _, err := s.RecordAbort("v1.0.0", 0, 6*time.Hour); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.RecordAbort("v1.0.0", ProgressResetBytes, 6*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Errorf("an attempt that made progress must impose no backoff, got %d", got)
	}
	if s.ShouldSkip("v1.0.0") {
		t.Error("must not skip after a productive attempt")
	}
}

func TestStore_NewTargetResetsLadder(t *testing.T) {
	s, _ := newTestStore(t)
	for i := 0; i < 3; i++ {
		if _, err := s.RecordAbort("v1.0.0", 0, 6*time.Hour); err != nil {
			t.Fatal(err)
		}
	}

	if s.ShouldSkip("v2.0.0") {
		t.Error("a different target must not inherit the old backoff")
	}
	got, err := s.RecordAbort("v2.0.0", 0, 6*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Errorf("new target starts at rung 1, got %d", got)
	}
}

func TestStore_ClearRemovesState(t *testing.T) {
	s, dir := newTestStore(t)
	if _, err := s.RecordAbort("v1.0.0", 0, 6*time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.Clear(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, stateFileName)); !os.IsNotExist(err) {
		t.Errorf("expected state file to be gone, got %v", err)
	}
	if s.ShouldSkip("v1.0.0") {
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
	if s.ShouldSkip("v1.0.0") {
		t.Error("unparseable state must fail open, not block updates")
	}
}

func TestStore_WriteIsAtomic(t *testing.T) {
	s, dir := newTestStore(t)
	if _, err := s.RecordAbort("v1.0.0", 0, 6*time.Hour); err != nil {
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
