package updater

import (
	"fmt"

	"github.com/librescoot/update-service/internal/dbcstate"
	"github.com/librescoot/update-service/internal/mender"
)

// rollbackGateArtifact differs from fallback cleanup: the trial image is still
// mounted. Verify that Mender will select the recorded original slot, and keep
// the attempt across the reboot that must land that selection.
func (u *Updater) rollbackGateArtifact(artifact string) error {
	obs, err := u.observeUpdate()
	if err != nil {
		return err
	}
	if obs.PendingArtifact != artifact || obs.State != mender.StateNeedsCommit {
		return fmt.Errorf("gate artifact is no longer pending")
	}
	if settled, err := u.reconcileActivation(obs); err != nil || settled {
		return fmt.Errorf("gate activation is not running (settled=%v): %v", settled, err)
	}
	a, err := dbcstate.LoadActivationAttempt(u.activationAttempt)
	if err != nil {
		return err
	}
	origin, err := u.activationOrigin(obs)
	if err != nil {
		return err
	}
	boot, err := u.bootState()
	if err != nil {
		return err
	}
	if a.Schema != 1 || a.Component != u.config.Component || origin.OriginalSlot != a.OriginalSlot ||
		boot.RunningSlot != a.TargetSlot || boot.OtherSlot != a.OriginalSlot || boot.SelectedSlot != a.TargetSlot || !boot.UpgradeAvailable {
		return fmt.Errorf("gate rollback destination is not verified")
	}
	a.Phase = "rolling-back"
	if err := dbcstate.SaveActivationAttempt(u.activationAttempt, a); err != nil {
		return err
	}
	if err := u.quarantineArtifact(artifact); err != nil {
		return err
	}
	if err := u.rollbackUpdate(); err != nil {
		return err
	}
	after, err := u.observeUpdate()
	if err != nil {
		return err
	}
	boot, err = u.bootState()
	if err != nil {
		return err
	}
	if after.State != mender.StateNoUpdate || after.PendingArtifact != "" || after.CommittedArtifact != a.CommittedArtifact ||
		boot.RunningSlot != a.TargetSlot || boot.OtherSlot != a.OriginalSlot || boot.SelectedSlot != a.OriginalSlot || boot.UpgradeAvailable {
		return fmt.Errorf("gate rollback did not select the original slot and clear pending state")
	}
	return nil
}
