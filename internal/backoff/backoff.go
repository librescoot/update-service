// Package backoff persists how often a component has abandoned a download of a
// given target, so a scooter on a hopeless link stops retrying every check.
//
// The state lives on /data rather than in Redis because Redis is wiped on
// every MDB reboot, and a ladder that resets on each boot never actually
// backs off. The ladder counts checks skipped rather than a wall-clock
// deadline, which additionally makes it immune to a wrong clock: a stale RTC
// reporting a plausible-but-wrong date used to make a stored deadline look
// far-future and wedge the ladder shut. There is no clock read anywhere in
// this package.
//
// The state stores a rung index, not a check count. A count computed once at
// record time and then stored would go stale the moment check-interval
// changes afterward: a rung recorded under a 30-minute interval as, say, 48
// checks still reads as 48 checks after the interval is restored to 6 hours,
// stranding the device in backoff for roughly 12 days instead of the
// intended one. The rung index is converted to a count fresh on every call
// that needs one, always against whatever check-interval is current at that
// moment.
package backoff

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

const stateFileName = ".download-state.json"

// ProgressResetBytes is how much an attempt must transfer to count as
// productive. A productive attempt resets the ladder even though it aborted:
// it was interrupted (suspend, modem loss, budget) rather than hopeless, and
// punishing it would park a link that is plainly working.
const ProgressResetBytes int64 = 1 << 20

// fallbackCheckInterval is substituted when the configured check interval is
// zero or negative (automatic checks disabled), purely to give the rung ->
// check-count conversion something sane to divide by. Only manual checks
// will decrement the resulting count in that case.
const fallbackCheckInterval = 6 * time.Hour

// rungs is the escalation ladder, expressed as durations and converted to a
// count of checks to skip whenever that count is actually needed. The last
// entry is the cap.
var rungs = []time.Duration{
	1 * time.Hour,
	3 * time.Hour,
	6 * time.Hour,
	24 * time.Hour,
}

// State is the on-disk record. One per component; the download directory is
// already per-component.
type State struct {
	Target      string `json:"target"`
	Aborts      int    `json:"aborts"`
	RungIndex   int    `json:"rung_index"`
	SkipsServed int    `json:"skips_served"`
}

type Store struct {
	path   string
	logger *log.Logger
}

func NewStore(dir string, logger *log.Logger) *Store {
	return &Store{
		path:   filepath.Join(dir, stateFileName),
		logger: logger,
	}
}

// load reads the state, failing open. A missing, truncated or unparseable file
// yields a zero State: one extra download attempt is a far better failure than
// updates wedged by a torn write.
func (s *Store) load() State {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			s.logger.Printf("Backoff state unreadable (%v), treating as absent", err)
		}
		return State{}
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		s.logger.Printf("Backoff state unparseable (%v), treating as absent", err)
		return State{}
	}
	return st
}

// rungAt returns the rung duration for idx, clamping to the valid range so a
// corrupt or out-of-range RungIndex from a torn write degrades to the nearest
// real rung instead of panicking.
func rungAt(idx int) time.Duration {
	if idx < 0 {
		idx = 0
	}
	if idx >= len(rungs) {
		idx = len(rungs) - 1
	}
	return rungs[idx]
}

// checksFor converts a rung duration into a count of checks to skip, given
// the cadence checks actually run at. checkInterval <= 0 means automatic
// checks are disabled, so only manual checks will ever decrement the count;
// fallbackCheckInterval is substituted so the conversion still yields a sane
// number instead of dividing by zero.
//
// The division rounds up: a rung that doesn't divide evenly into the
// interval must still be covered by the last check inside it, not left one
// check short. Duration is already integer nanoseconds, so this is plain
// integer ceiling division. checks is at least 1 for any positive rung.
func checksFor(rung, checkInterval time.Duration) int {
	if checkInterval <= 0 {
		checkInterval = fallbackCheckInterval
	}
	checks := (rung + checkInterval - 1) / checkInterval
	if checks < 1 {
		checks = 1
	}
	return int(checks)
}

// ChecksToSkip converts a rung index into a count of checks to skip at the
// given check interval. Exported so a caller that just recorded an abort -
// status.Reporter.SetAborted, immediately after RecordAbort - can publish
// the same total that ShouldSkip will later enforce, computed the same way.
// Both must agree, or the published count and the actually-served count
// drift apart.
func ChecksToSkip(rungIndex int, checkInterval time.Duration) int {
	return checksFor(rungAt(rungIndex), checkInterval)
}

// ShouldSkip reports whether an attempt at target should be deferred, and how
// many further checks remain backed off after this one. Serving the backoff
// is a side effect of asking: when it returns true it has also incremented
// and persisted SkipsServed, so each check spends exactly one skip. Call it
// at most once per check per target, or it will spend two.
//
// The total owed for the current rung is recomputed from checkInterval on
// every call rather than read back from a stored count, so a check-interval
// change takes effect on the very next check instead of leaving a device
// stranded on a total computed under an interval that no longer applies.
//
// A different target is never skipped: a new release deserves a fresh try.
func (s *Store) ShouldSkip(target string, checkInterval time.Duration) (skip bool, remaining int) {
	st := s.load()
	if st.Target != target || st.Aborts == 0 {
		return false, 0
	}

	total := ChecksToSkip(st.RungIndex, checkInterval)
	if st.SkipsServed >= total {
		return false, 0
	}

	st.SkipsServed++
	if err := s.write(st); err != nil {
		s.logger.Printf("Failed to persist decremented backoff state: %v", err)
	}
	remaining = total - st.SkipsServed
	s.logger.Printf("Skipping check for %s: download backed off, %d check(s) remaining", target, remaining)
	return true, remaining
}

// RecordAbort notes an abandoned attempt against target and returns the rung
// index the ladder now sits at, or -1 if the attempt's progress reset the
// ladder instead (no backoff applies). bytesGained is how much this attempt
// actually transferred; ProgressResetBytes or more resets rather than
// advances.
//
// This takes no check interval: the state stores a rung index, and the index
// is converted to a check count only at the moment it is used (ShouldSkip,
// ChecksToSkip), always against whatever checkInterval is current then.
func (s *Store) RecordAbort(target string, bytesGained int64) (int, error) {
	st := s.load()
	if st.Target != target {
		st = State{Target: target}
	}

	if bytesGained >= ProgressResetBytes {
		s.logger.Printf("Attempt for %s aborted but gained %d bytes, resetting backoff", target, bytesGained)
		st.Aborts = 0
		st.RungIndex = 0
		st.SkipsServed = 0
		if err := s.write(st); err != nil {
			return -1, err
		}
		return -1, nil
	}

	st.Aborts++
	st.RungIndex = min(st.Aborts-1, len(rungs)-1)
	st.SkipsServed = 0

	if err := s.write(st); err != nil {
		return -1, err
	}
	s.logger.Printf("Attempt %d for %s abandoned, now at rung %d of %d", st.Aborts, target, st.RungIndex+1, len(rungs))
	return st.RungIndex, nil
}

// Clear removes the state after a successful download.
func (s *Store) Clear() error {
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing backoff state: %w", err)
	}
	return nil
}

// write replaces the state atomically. A power cut mid-write must not leave a
// half-written file behind, and /data is on eMMC that loses page cache on a
// hard cut, so the temp file is fsynced before the rename.
func (s *Store) write(st State) error {
	data, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("marshalling backoff state: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".download-state-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp backoff state: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()

	// Close errors on these two paths are discarded on purpose: we are already
	// returning the error that actually matters, and the deferred Remove
	// cleans up the temp file either way.
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing temp backoff state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("syncing temp backoff state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp backoff state: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("renaming backoff state into place: %w", err)
	}
	tmpName = ""
	return nil
}
