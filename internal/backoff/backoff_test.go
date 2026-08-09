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

// drainSkips serves ShouldSkip repeatedly until it returns false, returning
// how many skips were granted at the given check interval.
func drainSkips(s *Store, target string, checkInterval time.Duration) int {
	n := 0
	for {
		skip, _ := s.ShouldSkip(target, checkInterval)
		if !skip {
			return n
		}
		n++
	}
}

func TestStore_NoStateMeansNoSkip(t *testing.T) {
	s, _ := newTestStore(t)
	if skip, _ := s.ShouldSkip("v1.0.0", 6*time.Hour); skip {
		t.Fatal("a component with no recorded aborts must not be skipped")
	}
}

func TestStore_RecordAbortAdvancesRungIndexThenCaps(t *testing.T) {
	s, _ := newTestStore(t)
	want := []int{0, 1, 2, 3, 3}
	for i, expected := range want {
		got, err := s.RecordAbort("v1.0.0", 0)
		if err != nil {
			t.Fatalf("abort %d: %v", i+1, err)
		}
		if got != expected {
			t.Errorf("abort %d: rung index = %d, want %d", i+1, got, expected)
		}
	}
}

func TestStore_LadderProgressionSixHourInterval(t *testing.T) {
	s, _ := newTestStore(t)
	// Under the 6h default check interval, the 1h/3h/6h rungs each convert to
	// a single skipped check and only the 24h cap converts to more than one.
	wantTotal := []int{1, 1, 1, 4}
	for i, total := range wantTotal {
		if _, err := s.RecordAbort("v1.0.0", 0); err != nil {
			t.Fatalf("abort %d: %v", i+1, err)
		}
		if got := drainSkips(s, "v1.0.0", 6*time.Hour); got != total {
			t.Errorf("abort %d: served %d skips, want %d", i+1, got, total)
		}
	}
}

func TestStore_LadderProgressionThirtyMinuteInterval(t *testing.T) {
	s, _ := newTestStore(t)
	// A shortened check interval must still honour the intended wall-clock
	// delay: each rung converts to more checks, not the same count.
	wantTotal := []int{2, 6, 12, 48}
	for i, total := range wantTotal {
		if _, err := s.RecordAbort("v1.0.0", 0); err != nil {
			t.Fatalf("abort %d: %v", i+1, err)
		}
		if got := drainSkips(s, "v1.0.0", 30*time.Minute); got != total {
			t.Errorf("abort %d: served %d skips, want %d", i+1, got, total)
		}
	}
}

func TestStore_CheckIntervalZeroFallsBackToSixHours(t *testing.T) {
	s, _ := newTestStore(t)
	// checkInterval 0 means automatic checks are disabled; the conversion
	// must still produce a sane count rather than dividing by zero.
	if _, err := s.RecordAbort("v1.0.0", 0); err != nil {
		t.Fatal(err)
	}
	if got := drainSkips(s, "v1.0.0", 0); got != 1 {
		t.Errorf("served %d skips, want 1 (the 6h fallback conversion of the 1h rung)", got)
	}
}

func TestStore_ShouldSkipDecrementsRemainingThenStops(t *testing.T) {
	s, _ := newTestStore(t)
	for i := 0; i < 4; i++ {
		if _, err := s.RecordAbort("v1.0.0", 0); err != nil {
			t.Fatal(err)
		}
	}
	// 4th abort lands on the 24h rung, 4 checks under a 6h interval.
	wantRemaining := []int{3, 2, 1, 0}
	for i, want := range wantRemaining {
		skip, remaining := s.ShouldSkip("v1.0.0", 6*time.Hour)
		if !skip {
			t.Fatalf("check %d: expected a skip", i+1)
		}
		if remaining != want {
			t.Errorf("check %d: remaining = %d, want %d", i+1, remaining, want)
		}
	}
	if skip, _ := s.ShouldSkip("v1.0.0", 6*time.Hour); skip {
		t.Error("expected no more skips once the count is exhausted")
	}
}

func TestStore_ShouldSkipPersistsAcrossInstances(t *testing.T) {
	s, dir := newTestStore(t)
	if _, err := s.RecordAbort("v1.0.0", 0); err != nil {
		t.Fatal(err)
	}
	if skip, _ := s.ShouldSkip("v1.0.0", 6*time.Hour); !skip {
		t.Fatal("expected a skip")
	}
	// A fresh Store pointed at the same file must see the decrement, not the
	// original count: that is what makes the ladder survive a process
	// restart, which is the entire reason state lives on /data.
	s2 := NewStore(dir, log.New(os.Stdout, "test: ", 0))
	if skip, _ := s2.ShouldSkip("v1.0.0", 6*time.Hour); skip {
		t.Error("expected the single skip already served to persist across instances")
	}
}

// TestStore_ShouldSkipRecomputesTotalOnIntervalRestore is the regression test
// for the bug this rewrite fixes: a count converted once at record time and
// then stored goes stale the moment check-interval changes. Serving skips at
// a short interval and then restoring the long default must not strand the
// device on a count computed under the short interval.
func TestStore_ShouldSkipRecomputesTotalOnIntervalRestore(t *testing.T) {
	s, _ := newTestStore(t)
	// Four aborts land on the 24h cap.
	for i := 0; i < 4; i++ {
		if _, err := s.RecordAbort("v1.0.0", 0); err != nil {
			t.Fatalf("abort %d: %v", i+1, err)
		}
	}

	// Serve 10 checks at a 30-minute interval, well short of the 48 the 24h
	// rung converts to at that cadence.
	for i := 0; i < 10; i++ {
		if skip, _ := s.ShouldSkip("v1.0.0", 30*time.Minute); !skip {
			t.Fatalf("skip %d at 30m interval: expected a skip", i+1)
		}
	}

	// Restore the 6h default. A stranded stored count would still owe 38
	// more skips (48-10) at 6h each, about 9.5 days. Recomputed fresh against
	// the current interval, the 24h rung is only 4 checks, and 10 have
	// already been served, so the backoff is already spent.
	if skip, remaining := s.ShouldSkip("v1.0.0", 6*time.Hour); skip {
		t.Fatalf("expected the backoff to already be spent after interval restore, got skip=true remaining=%d", remaining)
	}
}

func TestStore_ProgressResetsLadder(t *testing.T) {
	s, _ := newTestStore(t)
	for i := 0; i < 3; i++ {
		if _, err := s.RecordAbort("v1.0.0", 0); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.RecordAbort("v1.0.0", ProgressResetBytes)
	if err != nil {
		t.Fatal(err)
	}
	if got != -1 {
		t.Errorf("an attempt that made progress must report no rung, got %d", got)
	}
	if skip, _ := s.ShouldSkip("v1.0.0", 6*time.Hour); skip {
		t.Error("must not skip after a productive attempt")
	}
}

func TestStore_NewTargetResetsLadder(t *testing.T) {
	s, _ := newTestStore(t)
	for i := 0; i < 3; i++ {
		if _, err := s.RecordAbort("v1.0.0", 0); err != nil {
			t.Fatal(err)
		}
	}

	if skip, _ := s.ShouldSkip("v2.0.0", 6*time.Hour); skip {
		t.Error("a different target must not inherit the old backoff")
	}
	got, err := s.RecordAbort("v2.0.0", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Errorf("new target starts at rung 0, got %d", got)
	}
}

func TestStore_ClearRemovesState(t *testing.T) {
	s, dir := newTestStore(t)
	if _, err := s.RecordAbort("v1.0.0", 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Clear(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, stateFileName)); !os.IsNotExist(err) {
		t.Errorf("expected state file to be gone, got %v", err)
	}
	if skip, _ := s.ShouldSkip("v1.0.0", 6*time.Hour); skip {
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
	if skip, _ := s.ShouldSkip("v1.0.0", 6*time.Hour); skip {
		t.Error("unparseable state must fail open, not block updates")
	}
}

func TestStore_WriteIsAtomic(t *testing.T) {
	s, dir := newTestStore(t)
	if _, err := s.RecordAbort("v1.0.0", 0); err != nil {
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

func TestStore_ChecksToSkipClampsOutOfRangeIndex(t *testing.T) {
	if got := ChecksToSkip(-1, 6*time.Hour); got != 1 {
		t.Errorf("negative index should clamp to rung 0 (1h -> 1 check), got %d", got)
	}
	if got := ChecksToSkip(len(rungs)+5, 6*time.Hour); got != 4 {
		t.Errorf("out-of-range index should clamp to the last rung (24h -> 4 checks), got %d", got)
	}
}
