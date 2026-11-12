// Package dbcstate persists dashboard observations on the MDB and activation
// attempts on each component, independently of Redis and the rootfs slot.
package dbcstate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	DefaultPath              = "/data/ota/dbc-state.json"
	DefaultActivationAttempt = "/data/ota/dbc-activation-attempt"
)

type Snapshot struct {
	RunningVersion string    `json:"running_version"`
	TargetVersion  string    `json:"target_version,omitempty"`
	Status         string    `json:"status"`
	ObservedAt     time.Time `json:"observed_at"`
}

func Load(path string) (Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Snapshot{}, err
	}
	var snapshot Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return Snapshot{}, fmt.Errorf("decode DBC state cache: %w", err)
	}
	if snapshot.RunningVersion == "" {
		return Snapshot{}, fmt.Errorf("DBC state cache has no running version")
	}
	return snapshot, nil
}

// ActivationAttempt is shared by both component updaters. Schema zero is the
// historical DBC reboot marker; it needs local Mender evidence before migration.
type ActivationAttempt struct {
	Schema            int    `json:"schema,omitempty"`
	AttemptID         string `json:"attempt_id,omitempty"`
	Component         string `json:"component,omitempty"`
	Artifact          string `json:"artifact"`
	BootID            string `json:"boot_id"`
	CommittedArtifact string `json:"committed_artifact,omitempty"`
	OriginalSlot      string `json:"original_slot,omitempty"`
	TargetSlot        string `json:"target_slot,omitempty"`
	Phase             string `json:"phase,omitempty"`
	ObservedBootID    string `json:"observed_boot_id,omitempty"`
}

func LoadActivationAttempt(path string) (ActivationAttempt, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ActivationAttempt{}, err
	}
	var attempt ActivationAttempt
	if err := json.Unmarshal(data, &attempt); err != nil {
		return ActivationAttempt{}, fmt.Errorf("decode activation attempt: %w", err)
	}
	if err := validateAttempt(attempt); err != nil {
		return ActivationAttempt{}, err
	}
	return attempt, nil
}

func SaveActivationAttempt(path string, attempt ActivationAttempt) error {
	if err := validateAttempt(attempt); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".activation-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	data, err := json.Marshal(attempt)
	if err != nil {
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
	if err := os.Rename(name, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func validateAttempt(a ActivationAttempt) error {
	if a.Artifact == "" {
		return fmt.Errorf("activation attempt has no artifact")
	}
	if a.Schema == 0 && a.BootID != "" {
		return nil
	}
	if a.Schema != 1 || a.AttemptID == "" || (a.Component != "mdb" && a.Component != "dbc") ||
		a.CommittedArtifact == "" || a.OriginalSlot == "" || a.TargetSlot == "" || a.OriginalSlot == a.TargetSlot {
		return fmt.Errorf("incomplete activation attempt")
	}
	switch a.Phase {
	case "installing", "staged":
		if a.BootID == "" {
			return fmt.Errorf("activation attempt has no originating boot ID")
		}
	case "adopted", "rolling-back", "committed", "rolled-back", "not-staged":
	default:
		return fmt.Errorf("unknown activation phase %q", a.Phase)
	}
	return nil
}

func ClearActivationAttempt(path string) error {
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// Save atomically replaces path using a temporary file in the same directory,
// so a power loss cannot expose a partially written JSON document.
func Save(path string, snapshot Snapshot) error {
	if snapshot.RunningVersion == "" {
		return fmt.Errorf("refusing to cache an empty running version")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), ".dbc-state-*")
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
		return fmt.Errorf("sync DBC state cache directory: %w", err)
	}
	return nil
}
