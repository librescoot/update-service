// Package commitgate persists the commit gate's decision state.
//
// The gate decides whether a pending Mender artifact is committed or rolled
// back, and both outcomes outlive the process: the service restarts under
// systemd, and a rollback is only landed by the bootloader on the next boot.
// Two documents are kept, with different lifetimes:
//
//   - the marker, one per attempt. It records which artifact is waiting, which
//     boot began the wait, and whether a rollback has already been attempted
//     for it. A restart resumes an existing marker, and a second boot that
//     presents the same artifact is a failure rather than more time to wait.
//   - the quarantine, one list per component. It records artifacts a rollback
//     already rejected, so the next startup does not reinstall the image the
//     gate just discarded.
//
// Both are written atomically: a power loss cannot expose a partial document,
// and the packages that consume them treat a missing file as the empty state.
package commitgate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Marker state verdicts. A marker carries the verdict it reached, so a restart
// after a decision can tell "we already decided" from "we are still deciding".
const (
	VerdictWaiting       = "waiting"
	VerdictCommitted     = "committed"
	VerdictRolledBack    = "rolled-back"
	VerdictRollbackStuck = "rollback-stuck"
)

// Marker records one gated commit attempt.
type Marker struct {
	Artifact       string `json:"artifact"`
	PendingVersion string `json:"pending_version,omitempty"`
	// BootID is the kernel boot ID the wait started in. The window is scoped to
	// a single boot: an uncommitted slot is reverted by the bootloader on the
	// next boot attempt, so a different boot ID means this is no longer the
	// same activation.
	BootID string `json:"boot_id"`
	// FirstSeen is when this attempt began waiting, monotonic within the boot
	// that wrote it. It is never rewritten by a service restart, so a crash
	// loop cannot extend the window.
	FirstSeen time.Time `json:"first_seen"`
	// RollbackAttempted is set before asking Mender to roll back. On the next
	// boot it distinguishes "the rollback did not land" from a fresh attempt,
	// which is what stops the gate from rebooting forever.
	RollbackAttempted bool `json:"rollback_attempted,omitempty"`
	// Probes is the last observed state of each probe, keyed by name, so a
	// timeout can name the probe that never became true.
	Probes map[string]bool `json:"probes,omitempty"`
	// Verdict and Reason are the last decision, kept for post-mortem after the
	// marker stops being the live state.
	Verdict   string    `json:"verdict,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// DefaultMarkerPath is where the MDB and DBC keep their gate markers.
func DefaultMarkerPath(component string) string {
	return fmt.Sprintf("/data/ota/commit-gate-%s.json", component)
}

// DefaultQuarantinePath is where the rolled-back artifact list lives.
func DefaultQuarantinePath(component string) string {
	return fmt.Sprintf("/data/ota/commit-gate-quarantine-%s.json", component)
}

// Quarantine is the list of artifacts a rollback rejected.
type Quarantine struct {
	Artifacts []string `json:"artifacts"`
}

// Store reads and writes the marker and quarantine for one component.
type Store struct {
	markerPath     string
	quarantinePath string
}

// NewStore returns the store for a component at the default paths.
func NewStore(component string) *Store {
	return &Store{
		markerPath:     DefaultMarkerPath(component),
		quarantinePath: DefaultQuarantinePath(component),
	}
}

// NewStoreAt returns a store at explicit paths, for tests.
func NewStoreAt(markerPath, quarantinePath string) *Store {
	return &Store{markerPath: markerPath, quarantinePath: quarantinePath}
}

// Load returns the marker. A missing marker is reported as os.ErrNotExist with
// a zero marker, which is the normal state for a device that owes no commit.
func (s *Store) Load() (Marker, error) {
	data, err := os.ReadFile(s.markerPath)
	if err != nil {
		return Marker{}, err
	}
	var marker Marker
	if err := json.Unmarshal(data, &marker); err != nil {
		return Marker{}, fmt.Errorf("decode commit gate marker: %w", err)
	}
	if marker.Artifact == "" || marker.BootID == "" {
		return Marker{}, fmt.Errorf("commit gate marker is incomplete")
	}
	return marker, nil
}

// Save atomically replaces the marker.
func (s *Store) Save(marker Marker) error {
	if marker.Artifact == "" || marker.BootID == "" {
		return fmt.Errorf("refusing to save an incomplete commit gate marker")
	}
	return writeAtomic(s.markerPath, ".commit-gate-", marker)
}

// Clear removes the marker. A marker whose decision has been carried out has no
// further use: the next boot either owes nothing or opens a fresh attempt.
func (s *Store) Clear() error {
	if err := os.Remove(s.markerPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Quarantined reports whether artifact is on the rejected list.
func (s *Store) Quarantined(artifact string) (bool, error) {
	list, err := s.quarantineList()
	if err != nil {
		return false, err
	}
	for _, entry := range list {
		if entry == artifact {
			return true, nil
		}
	}
	return false, nil
}

// AddQuarantine records artifact as rejected. Adding an artifact already on the
// list is a no-op.
func (s *Store) AddQuarantine(artifact string) error {
	if artifact == "" {
		return fmt.Errorf("refusing to quarantine an empty artifact")
	}
	list, err := s.quarantineList()
	if err != nil {
		return err
	}
	for _, entry := range list {
		if entry == artifact {
			return nil
		}
	}
	return writeAtomic(s.quarantinePath, ".commit-gate-quarantine-", Quarantine{
		Artifacts: append(list, artifact),
	})
}

// PruneQuarantine drops entries for which keep returns false. Callers pass a
// predicate over the running version: a quarantined artifact at or below it can
// never become an install target again, so the entry has served its purpose.
func (s *Store) PruneQuarantine(keep func(artifact string) bool) error {
	list, err := s.quarantineList()
	if err != nil {
		return err
	}
	kept := make([]string, 0, len(list))
	for _, entry := range list {
		if keep(entry) {
			kept = append(kept, entry)
		}
	}
	if len(kept) == len(list) {
		return nil
	}
	if len(kept) == 0 {
		return s.clearQuarantine()
	}
	return writeAtomic(s.quarantinePath, ".commit-gate-quarantine-", Quarantine{Artifacts: kept})
}

func (s *Store) clearQuarantine() error {
	if err := os.Remove(s.quarantinePath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *Store) quarantineList() ([]string, error) {
	data, err := os.ReadFile(s.quarantinePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var list Quarantine
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("decode commit gate quarantine: %w", err)
	}
	return list.Artifacts, nil
}

// writeAtomic replaces path through a temporary file in the same directory,
// followed by a directory sync, so a power loss cannot expose a partial
// document.
func writeAtomic(path, prefix string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), prefix)
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync commit gate directory: %w", err)
	}
	return nil
}
