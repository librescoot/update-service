// Package dbcstate persists the last stable dashboard version/update facts on
// the MDB so they remain observable while the dashboard is powered off and
// after Redis data loss.
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

type ActivationAttempt struct {
	Artifact string `json:"artifact"`
	BootID   string `json:"boot_id"`
}

func LoadActivationAttempt(path string) (ActivationAttempt, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ActivationAttempt{}, err
	}
	var attempt ActivationAttempt
	if err := json.Unmarshal(data, &attempt); err != nil {
		return ActivationAttempt{}, fmt.Errorf("decode DBC activation attempt: %w", err)
	}
	if attempt.Artifact == "" || attempt.BootID == "" {
		return ActivationAttempt{}, fmt.Errorf("incomplete DBC activation attempt")
	}
	return attempt, nil
}

func SaveActivationAttempt(path string, attempt ActivationAttempt) error {
	if attempt.Artifact == "" || attempt.BootID == "" {
		return fmt.Errorf("refusing to save an incomplete activation attempt")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".dbc-activation-*")
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

func ClearActivationAttempt(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
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
