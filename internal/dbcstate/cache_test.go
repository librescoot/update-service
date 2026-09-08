package dbcstate

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dbc-state.json")
	want := Snapshot{
		RunningVersion: "v1.4.0",
		TargetVersion:  "v1.4.1",
		Status:         "pending-reboot",
		ObservedAt:     time.Date(2026, 9, 8, 6, 30, 0, 0, time.UTC),
	}
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestActivationAttemptRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "activation")
	want := ActivationAttempt{Artifact: "release-v1.4.0", BootID: "boot-a"}
	if err := SaveActivationAttempt(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadActivationAttempt(path)
	if err != nil || got != want {
		t.Fatalf("attempt = %+v, err=%v", got, err)
	}
	if err := ClearActivationAttempt(path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadActivationAttempt(path); !os.IsNotExist(err) {
		t.Fatalf("expected attempt removal, got %v", err)
	}
}

func TestSaveRejectsEmptyRunningVersion(t *testing.T) {
	if err := Save(filepath.Join(t.TempDir(), "dbc-state.json"), Snapshot{}); err == nil {
		t.Fatal("expected empty running version to be rejected")
	}
}
