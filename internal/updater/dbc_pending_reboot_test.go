package updater

import (
	"path/filepath"
	"testing"

	"github.com/librescoot/update-service/internal/mender"
)

// A DBC can still lose power before its local reboot completes. Its durable
// pending state must survive so MDB orchestration can power it back on and its
// own startup recovery can resume activation.
func TestPendingRebootSurvivesDashboardPowerOff(t *testing.T) {
	if dbcStateIsStaleOnPowerOff("pending-reboot") {
		t.Error("pending-reboot cleared on power-off: the staged image loses its status and the DBC boots into idle, skipping recoverFromStuckState")
	}
	if dbcStatusBlocksOrchestration("pending-reboot") {
		t.Error("pending-reboot treated as busy: nothing powers the DBC back on, so the staged image is stranded")
	}
}

func TestTriggerRebootDBCRunsLocalReboot(t *testing.T) {
	u, mr := newPendingCommitUpdater(t)
	u.config.Component = "dbc"
	u.activationAttempt = filepath.Join(t.TempDir(), "activation")
	u.bootID = func() (string, error) { return "boot-a", nil }
	u.observeUpdate = func() (mender.UpdateObservation, error) {
		return mender.UpdateObservation{PendingArtifact: "release-v1.4.0"}, nil
	}
	mr.HSet("vehicle", "state", "stand-by")
	called := false
	u.localReboot = func() error {
		called = true
		return nil
	}
	if err := u.TriggerReboot("dbc", true); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("local DBC reboot was not requested")
	}
}

func TestTriggerBootRebootDBCDoesNotRequireMender(t *testing.T) {
	u, mr := newPendingCommitUpdater(t)
	u.config.Component = "dbc"
	mr.HSet("vehicle", "state", "parked")
	called := false
	u.localReboot = func() error { called = true; return nil }
	if err := u.TriggerBootReboot("dbc", true); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("boot-only DBC reboot was not requested")
	}
}

func TestTriggerRebootMDBDefersToUMSOwner(t *testing.T) {
	u, mr := newPendingCommitUpdater(t)
	mr.HSet("ota", "reboot-owner:mdb", "ums")
	mr.HSet("vehicle", "state", "stand-by")
	if err := u.TriggerReboot("mdb", true); err != nil {
		t.Fatal(err)
	}
	got, err := mr.List("scooter:power")
	if err == nil && len(got) != 0 {
		t.Fatalf("update-service queued MDB reboot despite UMS ownership: %v", got)
	}
}

func TestDBCRebootAllowedState(t *testing.T) {
	for _, state := range []string{"stand-by", "parked", "shutting-down"} {
		if !dbcRebootAllowedState(state) {
			t.Errorf("state %q should allow DBC reboot", state)
		}
	}
	for _, state := range []string{"driving", "ready-to-drive", "", "hibernating"} {
		if dbcRebootAllowedState(state) {
			t.Errorf("state %q should not allow DBC reboot", state)
		}
	}
}

func TestDBCStateIsStaleOnPowerOff(t *testing.T) {
	tests := []struct {
		status string
		want   bool
	}{
		// Resume data lives on disk, so clearing these loses nothing.
		{"downloading", true},
		{"preparing", true},

		// Only the DBC can decide these.
		{"installing", false},
		{"pending-reboot", false},

		{"idle", false},
		{"error", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := dbcStateIsStaleOnPowerOff(tt.status); got != tt.want {
			t.Errorf("dbcStateIsStaleOnPowerOff(%q) = %v, want %v", tt.status, got, tt.want)
		}
	}
}

func TestDBCStatusBlocksOrchestration(t *testing.T) {
	tests := []struct {
		status string
		want   bool
	}{
		{"downloading", true},
		{"preparing", true},
		{"installing", true},
		{"error", true},

		{"pending-reboot", false},
		{"idle", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := dbcStatusBlocksOrchestration(tt.status); got != tt.want {
			t.Errorf("dbcStatusBlocksOrchestration(%q) = %v, want %v", tt.status, got, tt.want)
		}
	}
}
