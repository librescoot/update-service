// Package backoff persists how often a component has abandoned a download of a
// given target, so a scooter on a hopeless link stops retrying every check.
//
// The state lives on /data rather than in Redis because Redis is wiped on every
// MDB reboot, and a ladder that resets on each boot never actually backs off.
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

// sanityEpoch guards against honouring a retry deadline on a boot where the
// clock has not been set yet. Mirrors deltaSanityEpoch in internal/mender.
var sanityEpoch = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

// rungs is the escalation ladder. The last entry is the cap.
//
// Note these are a floor, not a schedule: retries only happen when a check
// runs, so the effective cadence is max(check-interval, rung). Under the 6h
// default check interval the first two rungs are inert; they exist for
// deployments that shorten it.
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
	RetryAfter int64  `json:"retry_after"`
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

// ShouldSkip reports whether an attempt at target should be deferred, and until
// when. A different target is never skipped: a new release deserves a fresh try.
func (s *Store) ShouldSkip(target string, now time.Time) (bool, time.Time) {
	if now.Before(sanityEpoch) {
		s.logger.Printf("Clock is before %s, ignoring download backoff", sanityEpoch.Format("2006-01-02"))
		return false, time.Time{}
	}
	st := s.load()
	if st.Target != target || st.RetryAfter == 0 {
		return false, time.Time{}
	}
	until := time.Unix(st.RetryAfter, 0)
	if now.Before(until) {
		return true, until
	}
	return false, time.Time{}
}

// RecordAbort notes an abandoned attempt and returns when the next one may
// start. A zero return means no backoff applies.
func (s *Store) RecordAbort(target string, bytesGained int64, now time.Time) (time.Time, error) {
	st := s.load()
	if st.Target != target {
		st = State{Target: target}
	}

	if bytesGained >= ProgressResetBytes {
		s.logger.Printf("Attempt for %s aborted but gained %d bytes, resetting backoff", target, bytesGained)
		st.Aborts = 0
		st.RetryAfter = 0
		if err := s.write(st); err != nil {
			return time.Time{}, err
		}
		return time.Time{}, nil
	}

	st.Aborts++
	idx := min(st.Aborts-1, len(rungs)-1)
	retryAfter := now.Add(rungs[idx])
	st.RetryAfter = retryAfter.Unix()

	if err := s.write(st); err != nil {
		return time.Time{}, err
	}
	s.logger.Printf("Attempt %d for %s abandoned, next attempt no earlier than %s",
		st.Aborts, target, retryAfter.Format(time.RFC3339))
	return retryAfter, nil
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
