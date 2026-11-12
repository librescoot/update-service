package updater

import (
	"errors"
	"os"
	"testing"

	"github.com/librescoot/update-service/internal/dbcstate"
	"github.com/librescoot/update-service/internal/mender"
	"github.com/librescoot/update-service/internal/status"
)

func TestActivationResumesVerifiedCrashStates(t *testing.T) {
	for _, scenario := range []string{"before-staging", "commit-leave", "commit-cleanup", "rollback-cleanup"} {
		t.Run(scenario, func(t *testing.T) {
			h := newActivationHarness(t, "mdb")
			phase := "staged"
			if scenario == "before-staging" {
				phase = "installing"
			}
			if scenario == "rollback-cleanup" {
				phase = "rolling-back"
			}
			h.attempt(t, phase)
			h.observation.State = mender.StateNeedsResume
			h.observation.MenderState = "Cleanup"
			switch scenario {
			case "before-staging":
				h.id = "boot-install"
				h.observation.MenderState = "Before_ArtifactInstall_Enter"
				h.u.rootfsOrigin = func() (mender.RootfsOrigin, error) { return mender.RootfsOrigin{}, mender.ErrNoRootfsOrigin }
			case "commit-leave":
				h.trialBoot()
				h.boot.UpgradeAvailable = false
				h.observation.MenderState = "Before_ArtifactCommit_Leave"
			case "commit-cleanup":
				h.trialBoot()
				h.boot.UpgradeAvailable = false
				h.u.rootfsOrigin = func() (mender.RootfsOrigin, error) { return mender.RootfsOrigin{}, mender.ErrNoRootfsOrigin }
			case "rollback-cleanup":
				h.observation.RolledBack = true
				h.u.rootfsOrigin = func() (mender.RootfsOrigin, error) { return mender.RootfsOrigin{}, mender.ErrNoRootfsOrigin }
			}
			resumes := 0
			h.u.resumeUpdate = func() error {
				resumes++
				if scenario == "before-staging" {
					h.u.rootfsOrigin = func() (mender.RootfsOrigin, error) { return h.origin, nil }
					h.observation.State = mender.StateNeedsCommit
					h.boot.SelectedSlot = "slot-b"
					h.boot.UpgradeAvailable = true
					return nil
				}
				if scenario == "rollback-cleanup" {
					return h.u.rollbackUpdate()
				}
				return h.u.commitUpdate()
			}
			rec, err := h.u.ReconcilePendingUpdate()
			if err != nil {
				t.Fatal(err)
			}
			if resumes != 1 {
				t.Fatalf("resume calls %d", resumes)
			}
			if (scenario == "before-staging") != rec.NeedsReboot {
				t.Fatalf("unexpected reboot result %+v", rec)
			}
		})
	}
}

func TestActivationResumeRejectsIncompleteEvidence(t *testing.T) {
	for _, scenario := range []string{"no-marker", "missing-header", "wrong-origin", "wrong-boot-selection", "failed-cleanup"} {
		t.Run(scenario, func(t *testing.T) {
			h := newActivationHarness(t, "mdb")
			if scenario != "no-marker" {
				h.attempt(t, "installing")
			}
			h.observation.State = mender.StateNeedsResume
			h.observation.MenderState = "Before_ArtifactInstall_Enter"
			switch scenario {
			case "missing-header":
				h.u.rootfsOrigin = func() (mender.RootfsOrigin, error) { return mender.RootfsOrigin{}, os.ErrNotExist }
			case "wrong-origin":
				h.origin.OriginalSlot = "slot-b"
			case "wrong-boot-selection":
				h.boot.SelectedSlot = "slot-a"
				h.boot.UpgradeAvailable = true
			case "failed-cleanup":
				h.trialBoot()
				h.boot.UpgradeAvailable = false
				h.observation.MenderState = "Cleanup"
				h.observation.Failed = true
			}
			h.u.resumeUpdate = func() error { t.Fatal("resumed ambiguous state"); return nil }
			if _, err := h.u.ReconcilePendingUpdate(); err == nil {
				t.Fatal("accepted incomplete resume evidence")
			}
		})
	}
}

func TestActivationDoesNotClearUnresolvedErrors(t *testing.T) {
	h := newActivationHarness(t, "mdb")
	h.attempt(t, "staged")
	if err := h.u.status.SetError(h.u.ctx, "recovery-failed", "unresolved"); err != nil {
		t.Fatal(err)
	}
	if err := h.u.recoverFromStuckState(false, false); err != nil {
		t.Fatal(err)
	}
	h.u.checkForUpdates(false)
	got, _ := h.u.status.GetStatus(h.u.ctx)
	if got != status.StatusError {
		t.Fatalf("cleared unresolved error: %s", got)
	}
	if _, err := dbcstate.LoadActivationAttempt(h.u.activationAttempt); err != nil {
		t.Fatal("lost evidence", err)
	}
}

func TestActivationRefusesChangedStateImmediatelyBeforeRollback(t *testing.T) {
	h := newActivationHarness(t, "mdb")
	h.attempt(t, "staged")
	calls := 0
	h.u.observeUpdate = func() (mender.UpdateObservation, error) {
		calls++
		o := h.observation
		if calls > 1 {
			o.PendingArtifact = "release-other"
		}
		return o, nil
	}
	if _, err := h.u.ReconcilePendingUpdate(); err == nil {
		t.Fatal("rolled back changed Mender identity")
	}
	if h.rollbacks != 0 {
		t.Fatal("rollback command executed after identity changed")
	}
}

func TestActivationCommitRequiresVerifiedBootSelection(t *testing.T) {
	h := newActivationHarness(t, "mdb")
	h.attempt(t, "staged")
	h.trialBoot()
	commit := h.u.commitUpdate
	h.u.commitUpdate = func() error { _ = commit(); h.boot.SelectedSlot = "slot-a"; return nil }
	if _, err := h.u.ReconcilePendingUpdate(); err == nil {
		t.Fatal("commit accepted wrong boot selection")
	}
	if _, err := dbcstate.LoadActivationAttempt(h.u.activationAttempt); err != nil {
		t.Fatal("commit lost evidence", err)
	}
	got, _ := h.u.status.GetStatus(h.u.ctx)
	if got != status.StatusError {
		t.Fatalf("reported %s", got)
	}
}

func TestGateRollbackFailureRetainsEvidenceAndDoesNotReboot(t *testing.T) {
	h := newGateHarness(t)
	h.updater.rollbackUpdate = func() error { h.rollbacks++; return errors.New("rollback failed") }
	marker, err := h.updater.openGateMarker(h.gated())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.updater.rollbackGatedUpdate(marker, "test failure"); err != nil {
		t.Fatal(err)
	}
	if h.reboots != 0 || h.rollbacks != 1 {
		t.Fatalf("unexpected actions reboot=%d rollback=%d", h.reboots, h.rollbacks)
	}
	a, err := dbcstate.LoadActivationAttempt(h.updater.activationAttempt)
	if err != nil || a.Phase != "rolling-back" {
		t.Fatalf("lost gate activation evidence: %+v %v", a, err)
	}
	if _, err := h.store.Load(); err != nil {
		t.Fatal("lost gate verdict", err)
	}
}
