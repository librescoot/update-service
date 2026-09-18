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
	u.rollbackUpdate = func() error { rolledBack = true; return nil }

	needsReboot, err := u.CheckAndCommitPendingUpdate()
	if err != nil {
		t.Fatalf("CheckAndCommitPendingUpdate returned error: %v", err)
	}
	if needsReboot {
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
	if got != status.StatusIdle {
		t.Fatalf("status after rollback = %q, want idle", got)
	}
}

// Same boot ID means the reboot never actually happened: keep the retry path
// (return needsReboot) and clear the marker so the next boot re-saves it.
func TestSameBootActivationRetriesReboot(t *testing.T) {
	u, _, marker := rolledBackUpdater(t, "boot-current")
	u.rollbackUpdate = func() error {
		t.Fatal("rollback must not run for a same-boot retry")
		return nil
	}

	needsReboot, err := u.CheckAndCommitPendingUpdate()
	if err != nil {
		t.Fatalf("CheckAndCommitPendingUpdate returned error: %v", err)
	}
	if !needsReboot {
		t.Fatal("same-boot activation should still request the reboot")
	}
	if _, err := dbcstate.LoadActivationAttempt(marker); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("same-boot marker should be cleared before retrying, got err=%v", err)
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

// A fresh install must not inherit an activation marker from an earlier
// attempt for the same artifact, or the next startup would finalise a rollback
// before the new activation is attempted.
func TestFreshInstallClearsStaleActivationMarker(t *testing.T) {
	events := []string{}
	u := newInstallGateTestUpdater("dbc", &fakeDBCInstallGuard{events: &events}, nil, &events)
	u.activationAttempt = filepath.Join(t.TempDir(), "dbc-activation-attempt")
	if err := dbcstate.SaveActivationAttempt(u.activationAttempt, dbcstate.ActivationAttempt{
		Artifact: pendingArtifact,
		BootID:   "boot-previous",
	}); err != nil {
		t.Fatal(err)
	}

	if err := u.installMender("/tmp/artifact.mender"); err != nil {
		t.Fatalf("installMender: %v", err)
	}
	if _, err := dbcstate.LoadActivationAttempt(u.activationAttempt); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale activation marker survived a fresh install, got err=%v", err)
	}
}
