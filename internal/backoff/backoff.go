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
// count of checks to skip at record time. The last entry is the cap.
var rungs = []time.Duration{
	1 * time.Hour,
	3 * time.Hour,
	6 * time.Hour,
	24 * time.Hour,
}

// State is the on-disk record. One per component; the download directory is
// already per-component.
type State struct {
	Target     string `json:"target"`
	Aborts     int    `json:"aborts"`
	SkipChecks int    `json:"skip_checks"`
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

// ShouldSkip reports whether an attempt at target should be deferred. Serving
// the backoff is a side effect of asking: when it returns true it has also
// decremented and persisted the remaining count, so each check spends exactly
// one skip. Call it at most once per check per target, or it will spend two.
//
// A different target is never skipped: a new release deserves a fresh try.
func (s *Store) ShouldSkip(target string) bool {
	st := s.load()
	if st.Target != target || st.SkipChecks <= 0 {
		return false
	}
	st.SkipChecks--
	if err := s.write(st); err != nil {
		s.logger.Printf("Failed to persist decremented backoff state: %v", err)
	}
	s.logger.Printf("Skipping check for %s: download backed off, %d check(s) remaining", target, st.SkipChecks)
	return true
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

// RecordAbort notes an abandoned attempt and returns how many subsequent
// checks to skip. A zero return means no backoff applies. checkInterval is
// the configured check cadence, used to convert the rung duration into a
// count of checks.
func (s *Store) RecordAbort(target string, bytesGained int64, checkInterval time.Duration) (int, error) {
	st := s.load()
	if st.Target != target {
		st = State{Target: target}
	}

	if bytesGained >= ProgressResetBytes {
		s.logger.Printf("Attempt for %s aborted but gained %d bytes, resetting backoff", target, bytesGained)
		st.Aborts = 0
		st.SkipChecks = 0
		if err := s.write(st); err != nil {
			return 0, err
		}
		return 0, nil
	}

	st.Aborts++
	idx := min(st.Aborts-1, len(rungs)-1)
	st.SkipChecks = checksFor(rungs[idx], checkInterval)

	if err := s.write(st); err != nil {
		return 0, err
	}
	s.logger.Printf("Attempt %d for %s abandoned, skipping the next %d check(s)",
		st.Aborts, target, st.SkipChecks)
	return st.SkipChecks, nil
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
