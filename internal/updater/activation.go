package updater

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"

	"github.com/librescoot/update-service/internal/dbcstate"
	"github.com/librescoot/update-service/internal/mender"
)

func activationAttemptID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}

func (u *Updater) hasUnresolvedActivation() bool {
	_, err := dbcstate.LoadActivationAttempt(u.activationAttempt)
	return !errors.Is(err, os.ErrNotExist)
}

func (u *Updater) recordInstallIntent(path string) error {
	if _, err := dbcstate.LoadActivationAttempt(u.activationAttempt); err == nil {
		return fmt.Errorf("an unresolved activation attempt already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	info, err := u.artifactInfo(path)
	if err != nil {
		return err
	}
	if info.ArtifactName == "" {
		return fmt.Errorf("artifact has no exact identity")
	}
	if u.gateStore == nil {
		return fmt.Errorf("activation quarantine is unavailable")
	}
	rejected, err := u.gateStore.Quarantined(info.ArtifactName)
	if err != nil {
		return err
	}
	if rejected {
		return fmt.Errorf("artifact %q is quarantined", info.ArtifactName)
	}
	obs, err := u.observeUpdate()
	if err != nil {
		return err
	}
	if obs.State != mender.StateNoUpdate || obs.PendingArtifact != "" || obs.CommittedArtifact == "" {
		return fmt.Errorf("Mender is not settled before install")
	}
	running, err := u.runningVersion()
	if err != nil || !sameObservedVersion(running, obs.CommittedVersion) {
		return fmt.Errorf("running version %q does not match committed artifact (%v)", running, err)
	}
	boot, err := u.bootState()
	if err != nil {
		return err
	}
	if !settledBoot(boot) {
		return fmt.Errorf("bootloader is not settled on the running slot")
	}
	id, err := u.bootID()
	if err != nil || id == "" {
		return fmt.Errorf("read originating boot ID: %q (%v)", id, err)
	}
	attemptID, err := activationAttemptID()
	if err != nil {
		return err
	}
	return dbcstate.SaveActivationAttempt(u.activationAttempt, dbcstate.ActivationAttempt{
		Schema: 1, AttemptID: attemptID, Component: u.config.Component, Artifact: info.ArtifactName, CommittedArtifact: obs.CommittedArtifact,
		BootID: id, OriginalSlot: boot.RunningSlot, TargetSlot: boot.OtherSlot, Phase: "installing",
	})
}

func (u *Updater) pendingImageRunning() (bool, error) {
	a, err := dbcstate.LoadActivationAttempt(u.activationAttempt)
	if err != nil {
		return false, err
	}
	b, err := u.bootState()
	if err != nil {
		return false, err
	}
	return a.Schema == 1 && b.RunningSlot == a.TargetSlot && b.SelectedSlot == a.TargetSlot && b.UpgradeAvailable, nil
}

func (u *Updater) completeActivation(artifact string) error {
	a, err := dbcstate.LoadActivationAttempt(u.activationAttempt)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if a.Artifact != artifact {
		return fmt.Errorf("committed artifact contradicts activation attempt")
	}
	if a.Schema == 1 {
		a.Phase = "committed"
		a.ObservedBootID, err = u.bootID()
		if err != nil {
			return err
		}
		if err := dbcstate.SaveActivationAttempt(u.activationAttempt+".last", a); err != nil {
			return err
		}
	}
	marker, haveGate, err := u.loadGateMarker()
	if err != nil {
		return err
	}
	if haveGate && marker.Artifact == artifact {
		if err := u.clearGateMarker(); err != nil {
			return err
		}
		u.publishGateState(gateStateCommitted, "", "")
	}
	return u.clearActivationMarker()
}

func settledBoot(b mender.BootState) bool {
	return b.RunningSlot != "" && b.OtherSlot != "" && b.RunningSlot != b.OtherSlot && b.SelectedSlot == b.RunningSlot && !b.UpgradeAvailable
}

func (u *Updater) activationOrigin(obs mender.UpdateObservation) (mender.RootfsOrigin, error) {
	origin, err := u.rootfsOrigin()
	if err != nil {
		return origin, err
	}
	if origin.Artifact != obs.PendingArtifact || origin.CommittedArtifact != obs.CommittedArtifact {
		return origin, fmt.Errorf("rootfs module identities do not match pending and committed artifacts")
	}
	return origin, nil
}

// reconcileActivation runs before either immediate commit or the optional gate.
// Its settled result means the attempt was fully reconciled, including failure.
func (u *Updater) reconcileActivation(obs mender.UpdateObservation) (bool, error) {
	attempt, err := dbcstate.LoadActivationAttempt(u.activationAttempt)
	have := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, u.pendingCommitError(err)
	}
	if !have && obs.PendingArtifact == "" {
		return false, nil
	}
	if obs.State == mender.StateInconsistent {
		return false, u.pendingCommitError(fmt.Errorf("Mender artifact is inconsistent"))
	}
	boot, err := u.bootState()
	if err != nil {
		return false, u.pendingCommitError(fmt.Errorf("read activation boot state: %w", err))
	}
	if boot.RunningSlot == "" || boot.OtherSlot == "" || boot.RunningSlot == boot.OtherSlot {
		return false, u.pendingCommitError(fmt.Errorf("invalid rootfs slots"))
	}
	if obs.PendingArtifact == "" {
		if obs.State != mender.StateNoUpdate || !settledBoot(boot) {
			return false, u.pendingCommitError(fmt.Errorf("activation marker remains with unsettled durable state"))
		}
		running, err := u.runningVersion()
		if err != nil || !sameObservedVersion(running, obs.CommittedVersion) {
			return false, u.pendingCommitError(fmt.Errorf("settled artifact is not verified running: %q (%v)", running, err))
		}
		if attempt.Schema == 0 {
			if obs.CommittedArtifact != attempt.Artifact {
				return false, u.pendingCommitError(fmt.Errorf("legacy activation marker cannot be reconciled without pending module evidence"))
			}
			return false, u.clearActivationMarker()
		}
		if attempt.Component != u.config.Component {
			return false, u.pendingCommitError(fmt.Errorf("activation component mismatch"))
		}
		if obs.CommittedArtifact == attempt.Artifact && boot.RunningSlot == attempt.TargetSlot && boot.OtherSlot == attempt.OriginalSlot {
			return true, u.finishVerifiedUpdate(obs.CommittedArtifact)
		}
		if obs.CommittedArtifact == attempt.CommittedArtifact && boot.RunningSlot == attempt.OriginalSlot && boot.OtherSlot == attempt.TargetSlot {
			phase := "not-staged"
			if attempt.Phase == "rolling-back" {
				phase = "rolled-back"
			}
			return true, u.finishFailedActivation(attempt, phase)
		}
		return false, u.pendingCommitError(fmt.Errorf("settled artifact and slot contradict activation record"))
	}
	if have && attempt.Artifact != obs.PendingArtifact {
		return false, u.pendingCommitError(fmt.Errorf("activation artifact does not match pending artifact"))
	}
	// An interrupted download can predate rootfs-image's orig-part record. A
	// saved pre-install intent permits resume only while still on that old slot.
	if obs.State == mender.StateNeedsResume {
		if !have || attempt.Schema != 1 || attempt.Component != u.config.Component || attempt.CommittedArtifact != obs.CommittedArtifact {
			return false, u.pendingCommitError(fmt.Errorf("interrupted install lacks exact activation evidence"))
		}
		// These post-commit states do not change boot selection. Cleanup can
		// outlive the module tree it deleted before the database transaction.
		postCommit := obs.MenderState == "Before_ArtifactCommit_Leave" || obs.MenderState == "ArtifactCommit_Leave"
		if (postCommit || obs.MenderState == "Cleanup") && !obs.Failed && settledBoot(boot) {
			running, err := u.runningVersion()
			if err != nil {
				return false, u.pendingCommitError(err)
			}
			if postCommit {
				origin, err := u.activationOrigin(obs)
				if err != nil || origin.OriginalSlot != attempt.OriginalSlot {
					return false, u.pendingCommitError(fmt.Errorf("post-commit origin cannot be verified: %v", err))
				}
			}
			if (!obs.RolledBack && boot.RunningSlot == attempt.TargetSlot && boot.OtherSlot == attempt.OriginalSlot && sameObservedVersion(running, obs.PendingVersion)) ||
				(obs.RolledBack && boot.RunningSlot == attempt.OriginalSlot && boot.OtherSlot == attempt.TargetSlot && sameObservedVersion(running, obs.CommittedVersion)) {
				return false, nil
			}
			return false, u.pendingCommitError(fmt.Errorf("cleanup image contradicts activation record"))
		}
		origin, originErr := u.activationOrigin(obs)
		if originErr != nil {
			if !errors.Is(originErr, mender.ErrNoRootfsOrigin) || !settledBoot(boot) || boot.RunningSlot != attempt.OriginalSlot || boot.OtherSlot != attempt.TargetSlot || attempt.Phase != "installing" {
				return false, u.pendingCommitError(fmt.Errorf("cannot verify interrupted rootfs origin: %w", originErr))
			}
		} else if origin.OriginalSlot != attempt.OriginalSlot {
			return false, u.pendingCommitError(fmt.Errorf("interrupted rootfs rollback destination contradicts activation record"))
		}
		if !((boot.RunningSlot == attempt.OriginalSlot && boot.OtherSlot == attempt.TargetSlot) ||
			(boot.RunningSlot == attempt.TargetSlot && boot.OtherSlot == attempt.OriginalSlot)) ||
			(boot.SelectedSlot != attempt.OriginalSlot && boot.SelectedSlot != attempt.TargetSlot) {
			return false, u.pendingCommitError(fmt.Errorf("interrupted install has contradictory slots"))
		}
		if (boot.UpgradeAvailable && boot.SelectedSlot != attempt.TargetSlot) ||
			(!boot.UpgradeAvailable && boot.SelectedSlot != attempt.OriginalSlot) {
			return false, u.pendingCommitError(fmt.Errorf("interrupted install has contradictory boot selection"))
		}
		running, err := u.runningVersion()
		expected := obs.CommittedVersion
		if boot.RunningSlot == attempt.TargetSlot {
			expected = obs.PendingVersion
		}
		if err != nil || !sameObservedVersion(running, expected) {
			return false, u.pendingCommitError(fmt.Errorf("interrupted install is not running the expected image: %q (%v)", running, err))
		}
		return false, nil
	}
	if obs.State != mender.StateNeedsCommit && obs.State != mender.StateNeedsReboot {
		return false, u.pendingCommitError(fmt.Errorf("unexpected pending activation state"))
	}
	origin, err := u.activationOrigin(obs)
	if err != nil {
		return false, u.pendingCommitError(fmt.Errorf("read rootfs rollback destination: %w", err))
	}
	if origin.OriginalSlot != boot.RunningSlot && origin.OriginalSlot != boot.OtherSlot {
		return false, u.pendingCommitError(fmt.Errorf("unknown original rootfs slot"))
	}
	if !have || attempt.Schema == 0 {
		// Do not invent an originating boot ID for installs made by older readers.
		previousID := attempt.BootID
		attemptID, err := activationAttemptID()
		if err != nil {
			return false, u.pendingCommitError(err)
		}
		attempt = dbcstate.ActivationAttempt{Schema: 1, AttemptID: attemptID, Component: u.config.Component, Artifact: obs.PendingArtifact,
			CommittedArtifact: obs.CommittedArtifact, BootID: previousID, OriginalSlot: origin.OriginalSlot, Phase: "adopted"}
		attempt.TargetSlot = boot.OtherSlot
		if boot.RunningSlot != origin.OriginalSlot {
			attempt.TargetSlot = boot.RunningSlot
		}
		if err := dbcstate.SaveActivationAttempt(u.activationAttempt, attempt); err != nil {
			return false, u.pendingCommitError(err)
		}
	}
	if attempt.Component != u.config.Component || attempt.CommittedArtifact != obs.CommittedArtifact || attempt.OriginalSlot != origin.OriginalSlot ||
		!((boot.RunningSlot == attempt.OriginalSlot && boot.OtherSlot == attempt.TargetSlot) || (boot.RunningSlot == attempt.TargetSlot && boot.OtherSlot == attempt.OriginalSlot)) {
		return false, u.pendingCommitError(fmt.Errorf("activation identities or slots contradict durable rootfs evidence"))
	}
	id, err := u.bootID()
	if err != nil || id == "" {
		return false, u.pendingCommitError(fmt.Errorf("read current boot ID: %q (%v)", id, err))
	}
	running, err := u.runningVersion()
	if err != nil {
		return false, u.pendingCommitError(err)
	}
	if boot.RunningSlot == attempt.TargetSlot && boot.SelectedSlot == attempt.TargetSlot && boot.UpgradeAvailable &&
		(attempt.BootID == "" || id != attempt.BootID) && sameObservedVersion(running, obs.PendingVersion) && attempt.Phase != "rolling-back" {
		return false, nil
	}
	if boot.RunningSlot == attempt.OriginalSlot && sameObservedVersion(running, obs.CommittedVersion) {
		if boot.SelectedSlot == attempt.TargetSlot && boot.UpgradeAvailable && (attempt.BootID == "" || id == attempt.BootID) && attempt.Phase != "rolling-back" {
			return false, nil
		}
		if settledBoot(boot) && (attempt.BootID == "" || id != attempt.BootID || attempt.Phase == "rolling-back") {
			return true, u.finalizeRolledBackActivation(obs)
		}
	}
	return false, u.pendingCommitError(fmt.Errorf("activation boot ID, running image and bootloader state disagree"))
}

// finishFailedActivation retains a compact durable outcome before releasing the
// active record. A crash between those writes is safely replayed at startup.
func (u *Updater) finishFailedActivation(a dbcstate.ActivationAttempt, phase string) error {
	if u.gateStore == nil {
		return u.pendingCommitError(fmt.Errorf("activation quarantine unavailable"))
	}
	if err := u.quarantineArtifact(a.Artifact); err != nil {
		return u.pendingCommitError(err)
	}
	a.Phase = phase
	id, err := u.bootID()
	if err != nil || id == "" {
		return u.pendingCommitError(fmt.Errorf("read outcome boot ID: %q (%v)", id, err))
	}
	a.ObservedBootID = id
	if err := dbcstate.SaveActivationAttempt(u.activationAttempt+".last", a); err != nil {
		return u.pendingCommitError(err)
	}
	if err := u.status.SetError(u.ctx, "activation-failed", fmt.Sprintf("activation of %s failed; running %s", a.Artifact, a.CommittedArtifact)); err != nil {
		return err
	}
	if u.config.Component == "dbc" {
		if err := u.redis.PushUpdateCommand("complete-dbc"); err != nil {
			return err
		}
	}
	marker, haveGate, err := u.loadGateMarker()
	if err != nil {
		return u.pendingCommitError(err)
	}
	if haveGate && marker.Artifact == a.Artifact {
		if err := u.clearGateMarker(); err != nil {
			return u.pendingCommitError(err)
		}
		u.publishGateState(gateStateRolledBack, "activation returned to the committed slot", "")
	}
	return u.clearActivationMarker()
}
