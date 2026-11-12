package updater

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/librescoot/update-service/internal/dbcstate"
	"github.com/librescoot/update-service/internal/mender"
	"github.com/librescoot/update-service/internal/status"
)

const (
	pendingArtifact   = "release-custom-nightly-20260917t065026"
	pendingVersion    = "custom-nightly-20260917t065026"
	committedArtifact = "release-nightly-20260917T063518"
	committedVersion  = "nightly-20260917t063518"
)

func rolledBackUpdater(t *testing.T, markerBootID string) (*Updater, *miniredis.Miniredis, string) {
	t.Helper()
	u, mr := newPendingCommitUpdater(t)
	u.config.Component = "dbc"
	u.activationAttempt = filepath.Join(t.TempDir(), "dbc-activation-attempt")
	u.bootID = func() (string, error) { return "boot-current", nil }
	u.observeUpdate = func() (mender.UpdateObservation, error) {
		return mender.UpdateObservation{
			State:             mender.StateNeedsCommit,
			PendingArtifact:   pendingArtifact,
			PendingVersion:    pendingVersion,
			CommittedArtifact: committedArtifact,
			CommittedVersion:  committedVersion,
		}, nil
	}
	u.runningVersion = func() (string, error) { return committedVersion, nil }
	u.rootfsOrigin = func() (mender.RootfsOrigin, error) {
		return mender.RootfsOrigin{OriginalSlot: "slot-a", Artifact: pendingArtifact, CommittedArtifact: committedArtifact}, nil
	}
	u.bootState = func() (mender.BootState, error) {
		b := mender.BootState{RunningSlot: "slot-a", OtherSlot: "slot-b", SelectedSlot: "slot-a"}
		if markerBootID == "boot-current" || markerBootID == "" {
			b.SelectedSlot = "slot-b"
			b.UpgradeAvailable = true
		}
		return b, nil
	}
	if markerBootID != "" {
		if err := dbcstate.SaveActivationAttempt(u.activationAttempt, dbcstate.ActivationAttempt{
			Artifact: pendingArtifact,
			BootID:   markerBootID,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return u, mr, u.activationAttempt
}

// A reboot that already happened and came back on the committed rootfs is a
// rollback, not a reason to reboot again (librescoot-rj87).
func TestRolledBackActivationFinalizesWithoutReboot(t *testing.T) {
	u, _, marker := rolledBackUpdater(t, "boot-previous")
	if err := u.status.SetPendingReboot(u.ctx); err != nil {
		t.Fatal(err)
	}
	rolledBack := false
	u.rollbackUpdate = func() error {
		rolledBack = true
		u.observeUpdate = func() (mender.UpdateObservation, error) {
			return mender.UpdateObservation{State: mender.StateNoUpdate, CommittedArtifact: committedArtifact, CommittedVersion: committedVersion}, nil
		}
		return nil
	}

	reconciliation, err := u.ReconcilePendingUpdate()
	if err != nil {
		t.Fatalf("ReconcilePendingUpdate returned error: %v", err)
	}
	if reconciliation.NeedsReboot {
		t.Fatal("rolled-back activation still requested a reboot")
	}
	if !rolledBack {
		t.Error("Mender rollback was not requested")
	}
	if _, err := dbcstate.LoadActivationAttempt(marker); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("activation marker should be gone after rollback, got err=%v", err)
	}
	got, err := u.status.GetStatus(u.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != status.StatusError {
		t.Fatalf("status after rollback = %q, want error", got)
	}
}

// Same boot ID with a staged target means activation is still owed. Retain
// the original evidence while retrying the reboot.
func TestSameBootActivationRetriesReboot(t *testing.T) {
	u, _, marker := rolledBackUpdater(t, "boot-current")
	u.rollbackUpdate = func() error {
		t.Fatal("rollback must not run for a same-boot retry")
		return nil
	}

	reconciliation, err := u.ReconcilePendingUpdate()
	if err != nil {
		t.Fatalf("ReconcilePendingUpdate returned error: %v", err)
	}
	if !reconciliation.NeedsReboot {
		t.Fatal("same-boot activation should still request the reboot")
	}
	if _, err := dbcstate.LoadActivationAttempt(marker); err != nil {
		t.Errorf("same-boot marker should survive retrying: %v", err)
	}
}

// A failed local reboot must not erase the marker: the next boot needs it to
// tell "retry" from "rolled back".
func TestTriggerRebootKeepsActivationMarkerOnLocalRebootError(t *testing.T) {
	u, mr, marker := rolledBackUpdater(t, "")
	u.rollbackUpdate = func() error { return nil }
	mr.HSet("vehicle", "state", "stand-by")
	u.localReboot = func() error { return errors.New("signal: terminated") }

	if err := u.TriggerReboot("dbc", false); err == nil {
		t.Fatal("expected the failed local reboot to return an error")
	}
	if _, err := dbcstate.LoadActivationAttempt(marker); err != nil {
		t.Fatalf("activation marker was cleared on a failed reboot: %v", err)
	}
}

// An unresolved earlier attempt must be reconciled before a fresh install.
func TestFreshInstallRejectsUnresolvedActivationMarker(t *testing.T) {
	events := []string{}
	u := newInstallGateTestUpdater(t, "dbc", &fakeDBCInstallGuard{events: &events}, nil, &events)
	u.activationAttempt = filepath.Join(t.TempDir(), "dbc-activation-attempt")
	if err := dbcstate.SaveActivationAttempt(u.activationAttempt, dbcstate.ActivationAttempt{
		Artifact: pendingArtifact,
		BootID:   "boot-previous",
	}); err != nil {
		t.Fatal(err)
	}

	if err := u.installMender("/tmp/artifact.mender"); err == nil {
		t.Fatal("install overwrote unresolved activation marker")
	}
	if _, err := dbcstate.LoadActivationAttempt(u.activationAttempt); err != nil {
		t.Errorf("unresolved marker was lost: %v", err)
	}
}
