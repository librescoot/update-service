package updater

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/librescoot/update-service/internal/commitgate"
	"github.com/librescoot/update-service/internal/config"
	"github.com/librescoot/update-service/internal/dbcstate"
	"github.com/librescoot/update-service/internal/mender"
	"github.com/librescoot/update-service/internal/version"
)

// commitGateTick is how often the gate re-evaluates its probes. The window is
// tens of minutes, so this only bounds how quickly a passing system commits and
// how quickly a failing one is caught; nothing in the gate is time-critical.
const commitGateTick = 15 * time.Second

// Probe names. They are stable strings: a timeout publishes the name of the
// probe that never became true, and those messages are read after the fact.
const (
	gateProbeUptime       = "uptime"
	gateProbeSystemd      = "systemd"
	gateProbeUnits        = "units"
	gateProbeVehicle      = "vehicle"
	gateProbePowerManager = "power-manager"
	gateProbeOTAStatus    = "ota-status"
)

// Error codes published to the ota error stream by the gate.
const (
	gateErrorRollback = "commit-gate-rollback"
	gateErrorStuck    = "commit-gate-stuck"
	gateErrorHeld     = "commit-gate-held"
)

// Gate verdicts, published in ota[commit-gate:<component>].
const (
	gateStateWaiting       = "waiting"
	gateStateCommitted     = "committed"
	gateStateRolledBack    = "rolled-back"
	gateStateRollbackStuck = "rollback-stuck"
	gateStateAbandoned     = "abandoned"
	gateStateDisabled      = "disabled-runtime"
)

// GatedCommit is a pending Mender artifact the gate owns the decision for.
type GatedCommit struct {
	Artifact       string
	PendingVersion string
}

// Reconciliation is the outcome of reconciling Mender's durable state with the
// running system.
type Reconciliation struct {
	// NeedsReboot reports that Mender holds an update waiting for a reboot that
	// this startup did not perform.
	NeedsReboot bool
	// Gated is set when a commit is owed and the commit gate must earn it.
	Gated *GatedCommit
}

// gateProbeResult is one probe's outcome. Detail is the human-readable reason,
// used verbatim in the timeout that rolls the update back.
type gateProbeResult struct {
	Name   string
	Passed bool
	Detail string
}

// ReconcilePendingUpdate reconciles Mender's durable state with the running
// system, and decides who owns a commit that is owed: the gate, or the ordinary
// immediate-commit path. Redis is an output of this reconciliation, not its
// source of truth.
func (u *Updater) ReconcilePendingUpdate() (Reconciliation, error) {
	gated, settled, err := u.reconcileCommitGate()
	if err != nil {
		return Reconciliation{}, err
	}
	if settled {
		return Reconciliation{}, nil
	}
	if gated != nil {
		return Reconciliation{Gated: gated}, nil
	}

	needsReboot, err := u.checkAndCommitPendingUpdate("")
	if err != nil {
		return Reconciliation{}, err
	}
	return Reconciliation{NeedsReboot: needsReboot}, nil
}

// reconcileCommitGate decides whether the gate owns the pending commit. It
// returns settled when a previous gated attempt has been closed out and there is
// nothing for the ordinary startup path to do, and a GatedCommit when the gate
// must now earn the commit.
//
// The component shows up in three places: which units are required, whether the
// MDB's power state is a health signal at all, and which finaliser closes out a
// window the bootloader reverted. The DBC additionally carries an
// activation-attempt marker from before its reboot, which the gate now owns the
// outcome of.
func (u *Updater) reconcileCommitGate() (*GatedCommit, bool, error) {
	cfg := u.config.CommitGateSettings()
	marker, haveMarker, err := u.loadGateMarker()
	if err != nil {
		return nil, false, u.pendingCommitError(fmt.Errorf("read commit gate marker: %w", err))
	}
	if !haveMarker && !cfg.Enabled {
		// Nothing to settle and nothing to open, so the ordinary path owns this
		// commit. Returning before observing keeps the ungated startup sequence
		// exactly as it was.
		return nil, false, nil
	}

	observation, err := u.observeUpdate()
	if err != nil {
		return nil, false, fmt.Errorf("observe Mender state: %w", err)
	}

	if !haveMarker {
		return u.openCommitGate(observation)
	}

	if marker.Artifact != observation.PendingArtifact {
		// Mender no longer holds the artifact this marker is about, or holds a
		// different one. Either way the marker describes an attempt that is not
		// the current one, and leaving it would make the next boot read its boot
		// ID as evidence about an unrelated artifact.
		u.logger.Printf("Discarding commit gate marker for %s: Mender's pending artifact is %q",
			marker.Artifact, observation.PendingArtifact)
		return nil, false, u.clearGateMarker()
	}

	bootID, err := u.bootID()
	if err != nil {
		return nil, false, u.pendingCommitError(fmt.Errorf("read boot ID: %w", err))
	}
	running, err := u.runningVersion()
	if err != nil {
		return nil, false, u.pendingCommitError(fmt.Errorf("read running version: %w", err))
	}

	if marker.BootID != bootID {
		// This boot is not the one that opened the window, with the same
		// artifact still pending. The window never spans a reboot: an
		// uncommitted slot is reverted by the bootloader on the next attempt, so
		// still running the pending artifact a boot later means no revert
		// happened.
		if sameObservedVersion(running, observation.CommittedVersion) {
			return nil, true, u.finalizeRevertedGate(marker, observation)
		}
		if marker.RollbackAttempted {
			return nil, true, u.finalizeStuckGate(marker,
				"the image is still running after a requested rollback")
		}
		return nil, true, u.rollbackGatedUpdate(marker,
			"the image rebooted before the gate reached a verdict")
	}

	if !sameObservedVersion(running, observation.PendingVersion) {
		// Same boot, committed slot: the reboot for this activation never
		// happened. The interrupted-reboot decision belongs to the ordinary
		// recovery path, which owns it for every boot, gated or not.
		u.logger.Printf("Commit gate window for %s did not reboot; leaving the decision to startup recovery", marker.Artifact)
		return nil, false, u.clearGateMarker()
	}

	// Resume: the process restarted inside the window, so the deadline still
	// runs from when the window opened and not from this startup.
	if err := u.reconstructPendingState(observation); err != nil {
		return nil, false, err
	}
	u.logger.Printf("Resuming the commit gate window for %s (opened %s, deadline %s)",
		marker.Artifact, marker.FirstSeen.Format(time.RFC3339), marker.FirstSeen.Add(cfg.Deadline).Format(time.RFC3339))
	return gatedCommitFor(observation), false, nil
}

// openCommitGate opens a window when the gate is enabled and the pending
// artifact is the one running.
func (u *Updater) openCommitGate(observation mender.UpdateObservation) (*GatedCommit, bool, error) {
	cfg := u.config.CommitGateSettings()
	if !cfg.Enabled || observation.State != mender.StateNeedsCommit || observation.PendingArtifact == "" {
		return nil, false, nil
	}
	running, err := u.runningVersion()
	if err != nil {
		// The ordinary path reports a running version it cannot read, with its
		// own error code and recovery.
		return nil, false, nil
	}
	if !sameObservedVersion(running, observation.PendingVersion) {
		return nil, false, nil
	}
	if err := u.reconstructPendingState(observation); err != nil {
		return nil, false, err
	}
	u.logger.Printf("Verified running version %s; deferring the commit of %s to the commit gate",
		running, observation.PendingArtifact)
	return gatedCommitFor(observation), false, nil
}

func gatedCommitFor(observation mender.UpdateObservation) *GatedCommit {
	return &GatedCommit{
		Artifact:       observation.PendingArtifact,
		PendingVersion: observation.PendingVersion,
	}
}

// startCommitGate runs the gate in the background for one gated attempt.
func (u *Updater) startCommitGate(gated *GatedCommit) {
	u.wg.Add(1)
	go func() {
		defer u.wg.Done()
		// The window runs for minutes with quiet stretches, so it is a long
		// operation like any other: prove this service is alive while it waits.
		defer u.startHeartbeat()()
		u.setGateWindowOpen(true)
		defer u.setGateWindowOpen(false)
		u.runCommitGate(gated)
	}()
}

// setGateWindowOpen records whether the gate is evaluating a pending artifact.
func (u *Updater) setGateWindowOpen(open bool) {
	u.gateWindowMu.Lock()
	u.gateWindowOpen = open
	u.gateWindowMu.Unlock()
}

// gateWindowIsOpen reports whether the gate is evaluating a pending artifact.
// While it is, an install attempt on top of that artifact fails with "already in
// progress", and the status such a failure publishes is the same status the
// gate's own ota-status probe reads.
func (u *Updater) gateWindowIsOpen() bool {
	u.gateWindowMu.RLock()
	defer u.gateWindowMu.RUnlock()
	return u.gateWindowOpen
}

// runCommitGate drives one gated attempt to a verdict.
func (u *Updater) runCommitGate(gated *GatedCommit) {
	marker, err := u.openGateMarker(gated)
	if err != nil {
		// Without a marker the verdict cannot be recorded, and the next boot
		// could not tell an unlanded rollback from a fresh attempt. Hold the
		// pending state and report rather than risk a reboot loop.
		u.logger.Printf("Commit gate cannot open a window for %s: %v", gated.Artifact, err)
		u.publishGateState(gateErrorHeld, "", err.Error())
		if statusErr := u.status.SetError(u.ctx, gateErrorHeld, fmt.Sprintf("cannot open commit gate: %v", err)); statusErr != nil {
			u.logger.Printf("Also failed to publish the commit gate error: %v", statusErr)
		}
		return
	}

	deadline := marker.FirstSeen.Add(u.config.CommitGateSettings().Deadline)
	u.logger.Printf("Commit gate waiting on %s until %s", gated.Artifact, deadline.Format(time.RFC3339))
	u.publishGateWaiting(marker, deadline, "")

	ticker := time.NewTicker(commitGateTick)
	defer ticker.Stop()
	for {
		if u.commitGateStep(&marker, gated) {
			return
		}
		select {
		case <-u.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// openGateMarker loads the marker this window is running under, creating it when
// the window is new. A marker from the same boot for the same artifact is
// resumed unchanged, so a service restart cannot extend the deadline.
func (u *Updater) openGateMarker(gated *GatedCommit) (commitgate.Marker, error) {
	bootID, err := u.bootID()
	if err != nil {
		return commitgate.Marker{}, fmt.Errorf("read boot ID: %w", err)
	}

	existing, haveMarker, err := u.loadGateMarker()
	if err != nil {
		return commitgate.Marker{}, err
	}
	if haveMarker && existing.Artifact == gated.Artifact && existing.BootID == bootID {
		return existing, nil
	}

	marker := commitgate.Marker{
		Artifact:       gated.Artifact,
		PendingVersion: gated.PendingVersion,
		BootID:         bootID,
		FirstSeen:      u.gateClock(),
		Verdict:        commitgate.VerdictWaiting,
	}
	if err := u.saveGateMarker(marker); err != nil {
		return commitgate.Marker{}, err
	}
	return marker, nil
}

// commitGateStep evaluates the probes once and acts on the outcome. done reports
// that the gate reached a verdict and the loop should stop.
func (u *Updater) commitGateStep(marker *commitgate.Marker, gated *GatedCommit) bool {
	cfg := u.config.CommitGateSettings()

	if !cfg.Enabled {
		// The kill switch was flipped while the window was open. Gating is off,
		// so the commit is owed and nothing is left to wait for.
		u.logger.Printf("Commit gate disabled at runtime; committing %s without a health verdict", gated.Artifact)
		u.commitGatedUpdate(marker, gated, gateStateDisabled)
		return true
	}

	probes := u.gateProbes(cfg)
	// Write the marker only when the observed probe set changes. The window is
	// twenty minutes of fifteen-second ticks and this is persistent storage, so
	// the same reason the DBC state cache is throttled applies here. Verdicts are
	// always written.
	passes := probePassMap(probes)
	if !probePassesEqual(marker.Probes, passes) {
		marker.Probes = passes
		marker.UpdatedAt = u.gateClock()
		if err := u.saveGateMarker(*marker); err != nil {
			// Best effort while waiting: the live state is still recoverable from
			// Mender, so a diagnostic write failure is not grounds to fail an
			// otherwise healthy update. The rollback path treats a write failure
			// differently, because there the record is what stops a reboot loop.
			u.logger.Printf("Commit gate cannot record probe state: %v", err)
		}
	}

	if name, detail := firstFailingProbe(probes); name != "" {
		failing := fmt.Sprintf("%s: %s", name, detail)
		deadline := marker.FirstSeen.Add(cfg.Deadline)
		u.publishGateWaiting(*marker, deadline, failing)
		if !u.gateClock().Before(deadline) {
			u.rollbackGatedUpdate(*marker, fmt.Sprintf("no health verdict within %v: %s", cfg.Deadline, failing))
			return true
		}
		return false
	}

	u.commitGatedUpdate(marker, gated, gateStateCommitted)
	return true
}

// commitGatedUpdate earns the commit: the probes held, so verify the artifact is
// still the one Mender holds and commit it.
func (u *Updater) commitGatedUpdate(marker *commitgate.Marker, gated *GatedCommit, verdict string) {
	observation, err := u.observeUpdate()
	if err != nil {
		u.rollbackGatedUpdate(*marker, fmt.Sprintf("cannot observe Mender before the gated commit: %v", err))
		return
	}
	if observation.PendingArtifact != gated.Artifact {
		// Mender moved on under the gate. Committing now would commit something
		// this gate did not judge, and rolling back would revert a decision that
		// is no longer ours to make.
		u.logger.Printf("Commit gate abandoning %s: Mender's pending artifact is now %q",
			gated.Artifact, observation.PendingArtifact)
		u.abandonCommitGate(*marker, verdict)
		return
	}
	running, err := u.runningVersion()
	if err != nil || !sameObservedVersion(running, observation.PendingVersion) {
		u.rollbackGatedUpdate(*marker,
			fmt.Sprintf("the running version changed under the gate (running=%q pending=%q err=%v)", running, observation.PendingVersion, err))
		return
	}

	if err := u.commitVerifiedUpdate(observation); err != nil {
		u.logger.Printf("Commit gate commit failed for %s: %v", gated.Artifact, err)
		u.rollbackGatedUpdate(*marker, fmt.Sprintf("the gated commit failed: %v", err))
		return
	}
	if err := u.clearGateMarker(); err != nil {
		u.logger.Printf("Commit gate committed %s but could not clear its marker: %v", gated.Artifact, err)
	}
	u.publishGateState(verdict, "", "")
	u.logger.Printf("Commit gate committed %s", gated.Artifact)
}

// rollbackGatedUpdate fails the attempt closed: record the decision so the next
// boot can tell an unlanded rollback from a fresh attempt, ask Mender to discard
// the pending update, quarantine the artifact so it is not reinstalled, then ask
// for the reboot that lands the rollback. A failure to carry out or record the
// rollback is handled here rather than returned: the caller has already stopped
// and the vehicle must not be left without a decision.
func (u *Updater) rollbackGatedUpdate(marker commitgate.Marker, reason string) error {
	marker.RollbackAttempted = true
	marker.Verdict = commitgate.VerdictRolledBack
	marker.Reason = reason
	marker.UpdatedAt = u.gateClock()
	if err := u.saveGateMarker(marker); err != nil {
		u.logger.Printf("Commit gate cannot record its rollback (%v); holding instead of rebooting", err)
		return u.holdGatedUpdate(marker, reason, "cannot record the rollback attempt")
	}

	if err := u.quarantineArtifact(marker.Artifact); err != nil {
		u.logger.Printf("Commit gate cannot quarantine %s: %v", marker.Artifact, err)
	}
	if err := u.rollbackUpdate(); err != nil {
		// Mender may have settled the standalone state already; the reboot and
		// the boot-ID comparison on the next boot are what decide the outcome.
		u.logger.Printf("Mender rollback after a failed gate window: %v", err)
	}
	if err := u.status.SetError(u.ctx, gateErrorRollback, reason); err != nil {
		u.logger.Printf("Failed to publish the commit gate rollback: %v", err)
	}
	u.publishGateState(gateStateRolledBack, reason, "")
	u.logger.Printf("Commit gate rolling back %s after a reboot: %s", marker.Artifact, reason)

	if err := u.gateRebootFunc(); err != nil {
		// A refusal here is not fatal: the slot is still uncommitted, so the
		// bootloader reverts it at the next power cycle, and the marker stands
		// as the record that this was a decision rather than a crash.
		u.logger.Printf("Commit gate could not request a reboot (%v); the rollback lands at the next power cycle", err)
	}
	return nil
}

// finalizeRevertedGate closes out a window the bootloader ended by reverting to
// the previously committed slot. Mender's standalone state is stale in that
// case, and nothing else can clear it: the generic recovery path answers
// "reboot still required", and no MDB path ever reboots.
func (u *Updater) finalizeRevertedGate(marker commitgate.Marker, observation mender.UpdateObservation) error {
	if err := u.quarantineArtifact(marker.Artifact); err != nil {
		u.logger.Printf("Commit gate cannot quarantine the reverted artifact %s: %v", marker.Artifact, err)
	}
	if u.config.Component == "dbc" {
		// The activation-attempt marker from before the reboot is the other half
		// of this window's record, and the DBC finaliser already owns closing it:
		// it rolls Mender back, clears that marker and publishes idle together.
		if err := u.finalizeRolledBackActivation(observation); err != nil {
			return err
		}
	} else {
		if err := u.rollbackUpdate(); err != nil {
			u.logger.Printf("Mender rollback after a reverted gate window: %v", err)
		}
		if err := u.status.SetIdle(u.ctx); err != nil {
			return u.pendingCommitError(fmt.Errorf("publish reverted gate window: %w", err))
		}
	}
	if err := u.clearGateMarker(); err != nil {
		return u.pendingCommitError(fmt.Errorf("clear reverted commit gate marker: %w", err))
	}
	u.publishGateState(gateStateRolledBack, "the bootloader reverted to the committed slot", "")
	u.logger.Printf("Commit gate: %s reverted to %s; not rebooting", marker.Artifact, observation.CommittedArtifact)
	return nil
}

// finalizeStuckGate ends an attempt whose rollback never landed: the vehicle is
// running the image the gate rejected. Rebooting again would loop, so clear the
// pending state, keep the artifact out of reinstalls, and report.
func (u *Updater) finalizeStuckGate(marker commitgate.Marker, reason string) error {
	if err := u.holdGatedUpdate(marker, reason, reason); err != nil {
		return err
	}
	u.publishGateState(gateStateRollbackStuck, reason, "")
	return nil
}

// holdGatedUpdate is the outcome for a rollback that cannot be carried out or
// recorded: stop, keep the artifact quarantined, and leave an error status that
// the next startup clears so the artifact can be retried by a newer release.
func (u *Updater) holdGatedUpdate(marker commitgate.Marker, reason, detail string) error {
	if err := u.quarantineArtifact(marker.Artifact); err != nil {
		u.logger.Printf("Commit gate cannot quarantine %s: %v", marker.Artifact, err)
	}
	if err := u.rollbackUpdate(); err != nil {
		u.logger.Printf("Mender rollback while holding a gated update: %v", err)
	}
	if err := u.clearGateMarker(); err != nil {
		u.logger.Printf("Commit gate cannot clear its marker: %v", err)
	}
	// The gate has decided this activation's outcome, so the DBC's record of
	// having asked for it must go too: left behind it would outlive its purpose
	// and, being read only in the pending-commit branch, would never be examined
	// again either.
	if err := u.clearActivationMarker(); err != nil {
		u.logger.Printf("Commit gate cannot clear the DBC activation attempt: %v", err)
	}
	message := reason
	if detail != "" && detail != reason {
		message = fmt.Sprintf("%s: %s", reason, detail)
	}
	if err := u.status.SetError(u.ctx, gateErrorStuck, message); err != nil {
		u.logger.Printf("Failed to publish the held commit gate state: %v", err)
	}
	u.logger.Printf("Commit gate holding %s without rebooting: %s", marker.Artifact, message)
	return nil
}

// abandonCommitGate drops a window whose artifact Mender no longer holds. The
// decision is no longer this gate's to make, so nothing is committed or reverted.
func (u *Updater) abandonCommitGate(marker commitgate.Marker, verdict string) {
	if err := u.clearGateMarker(); err != nil {
		u.logger.Printf("Commit gate cannot clear its marker: %v", err)
	}
	u.publishGateState(gateStateAbandoned, "Mender no longer holds this artifact", "")
	if verdict == gateStateDisabled {
		u.logger.Printf("Commit gate dropped %s without a verdict", marker.Artifact)
	}
}

// pruneCommitGateQuarantine drops quarantined artifacts the vehicle has moved
// past, and only those: an entry at or above the running version can still be
// offered as an update, and after a rollback the rejected artifact is exactly
// the version just above the one now running.
func (u *Updater) pruneCommitGateQuarantine() {
	if u.gateStore == nil {
		return
	}
	running, err := u.runningVersion()
	if err != nil || running == "" {
		return
	}
	err = u.gateStore.PruneQuarantine(func(artifact string) bool {
		quarantined := mender.VersionFromArtifact(artifact)
		if quarantined == "" {
			// Unrecognisable artifact names are kept: a name this cannot parse
			// is not evidence that the artifact is behind us.
			return true
		}
		return version.Compare(strings.ToLower(quarantined), strings.ToLower(running)) >= 0
	})
	if err != nil {
		u.logger.Printf("Cannot prune the commit gate quarantine: %v", err)
	}
}

// clearActivationMarker closes the DBC activation-attempt record. The gate owns
// that record's outcome while a window is open, and there is no such record on
// the MDB.
func (u *Updater) clearActivationMarker() error {
	if u.config.Component != "dbc" {
		return nil
	}
	return dbcstate.ClearActivationAttempt(u.activationAttempt)
}

// quarantineArtifact records an artifact the gate rejected.
func (u *Updater) quarantineArtifact(artifact string) error {
	if u.gateStore == nil || artifact == "" {
		return nil
	}
	return u.gateStore.AddQuarantine(artifact)
}

// gateQuarantinedVersions returns the versions the gate rolled back as a set of
// lowercase version tokens. The quarantine stores artifact names, because that is
// the identity the gate works with, while update candidates are named by
// version; this is the one place that translation happens.
func (u *Updater) gateQuarantinedVersions() map[string]bool {
	if u.gateStore == nil {
		return nil
	}
	entries, err := u.gateStore.List()
	if err != nil {
		u.logger.Printf("Cannot read the commit gate quarantine: %v", err)
		return nil
	}
	if len(entries) == 0 {
		return nil
	}
	versions := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if v := mender.VersionFromArtifact(entry); v != "" {
			versions[strings.ToLower(v)] = true
		}
	}
	return versions
}

// withoutGateRejectedReleases drops the releases this component's gate already
// rolled back. Reinstalling one would reproduce exactly the image that failed
// and be rolled back again, and because the quarantine names a single artifact
// rather than a channel, a newer release is unaffected.
func (u *Updater) withoutGateRejectedReleases(releases []Release) []Release {
	rejected := u.gateQuarantinedVersions()
	if len(rejected) == 0 {
		return releases
	}
	kept := make([]Release, 0, len(releases))
	for _, release := range releases {
		if rejected[strings.ToLower(release.TagName)] {
			u.logger.Printf("Skipping release %s: the commit gate rolled it back", release.TagName)
			continue
		}
		kept = append(kept, release)
	}
	return kept
}

// gateRebootFunc asks for the reboot that lands a rollback. TriggerReboot is the
// production path because it waits for a vehicle state that is safe to reboot
// from, and yields to a reboot another service owns.
func (u *Updater) gateRebootFunc() error {
	if u.gateReboot != nil {
		return u.gateReboot()
	}
	return u.TriggerReboot(u.config.Component, true)
}

// publishGateWaiting reports which probe is outstanding and when the window
// expires.
func (u *Updater) publishGateWaiting(marker commitgate.Marker, deadline time.Time, detail string) {
	reason := detail
	if reason == "" {
		reason = "probes not yet evaluated"
	}
	if err := u.status.SetCommitGate(u.ctx, gateStateWaiting, reason, deadline); err != nil {
		u.logger.Printf("Failed to publish the commit gate window: %v", err)
	}
}

// publishGateState reports a settled verdict alongside the error status that
// carries its code and message.
func (u *Updater) publishGateState(state, reason, detail string) {
	message := reason
	if message == "" {
		message = detail
	}
	if err := u.status.SetCommitGate(u.ctx, state, message, time.Time{}); err != nil {
		u.logger.Printf("Failed to publish the commit gate verdict: %v", err)
	}
}

// loadGateMarker reads the durable marker. A process without a store has never
// been configured for gating, which is the state the tests that do not exercise
// the gate run in, so this reports no marker rather than reaching for a default
// path.
func (u *Updater) loadGateMarker() (commitgate.Marker, bool, error) {
	if u.gateStore == nil {
		return commitgate.Marker{}, false, nil
	}
	marker, err := u.gateStore.Load()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return commitgate.Marker{}, false, nil
		}
		return commitgate.Marker{}, false, err
	}
	return marker, true, nil
}

func (u *Updater) saveGateMarker(marker commitgate.Marker) error {
	if u.gateStore == nil {
		return fmt.Errorf("no commit gate store is configured")
	}
	return u.gateStore.Save(marker)
}

func (u *Updater) clearGateMarker() error {
	if u.gateStore == nil {
		return nil
	}
	if err := u.gateStore.Clear(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// gateClock is the wall clock the window is measured against. It is injectable
// because the deadline is minutes long and the tests drive it directly.
func (u *Updater) gateClock() time.Time {
	if u.gateNow == nil {
		return time.Now()
	}
	return u.gateNow()
}

// gateProbes evaluates the probes, unless a test supplied its own evaluation.
func (u *Updater) gateProbes(cfg config.CommitGateSettings) []gateProbeResult {
	if u.gateEvaluate != nil {
		return u.gateEvaluate(cfg)
	}
	return u.evaluateCommitGateProbes(cfg)
}

// evaluateCommitGateProbes observes the system once. Order is the priority
// order for reporting: the first failure is the one a timeout names.
func (u *Updater) evaluateCommitGateProbes(cfg config.CommitGateSettings) []gateProbeResult {
	uptime, uptimeErr := u.gateProbeUptime()
	uptimeDetail := fmt.Sprintf("up to %v, need %v", uptime.Round(time.Second), cfg.Floor)
	if uptimeErr != nil {
		uptimeDetail = fmt.Sprintf("cannot read uptime: %v", uptimeErr)
	}

	systemdState, systemdErr := u.gateProbeSystemdState()
	systemdDetail := fmt.Sprintf("systemd state %q", systemdState)
	if systemdErr != nil {
		systemdDetail = fmt.Sprintf("cannot read systemd state: %v", systemdErr)
	}

	inactiveUnit, unitsErr := u.gateProbeUnits(cfg.RequiredUnits)
	unitsDetail := "all required units satisfied"
	if unitsErr != nil {
		unitsDetail = fmt.Sprintf("cannot read unit state: %v", unitsErr)
	} else if inactiveUnit != "" {
		unitsDetail = fmt.Sprintf("%s is neither active nor has it run successfully this boot", inactiveUnit)
	}

	vehicleState, vehicleErr := u.gateProbeVehicleState()
	vehicleDetail := fmt.Sprintf("vehicle state %q", vehicleState)
	if vehicleErr != nil {
		vehicleDetail = fmt.Sprintf("cannot read vehicle state: %v", vehicleErr)
	}

	otaStatus, otaDetail := u.gateProbeOTAStatus()

	results := []gateProbeResult{
		{Name: gateProbeUptime, Passed: uptimeErr == nil && uptime >= cfg.Floor, Detail: uptimeDetail},
		{Name: gateProbeSystemd, Passed: systemdErr == nil && systemdRunning(systemdState), Detail: systemdDetail},
		{Name: gateProbeUnits, Passed: unitsErr == nil && inactiveUnit == "", Detail: unitsDetail},
		{Name: gateProbeVehicle, Passed: vehicleErr == nil && vehicleState != "", Detail: vehicleDetail},
	}

	// The MDB's own power state is a health signal for the MDB and only for the
	// MDB: pm-service and the hash it publishes to both live there. On the DBC
	// that hash is the MDB's, whose power state may legitimately leave "running"
	// while the DBC is still evaluating — it holds dashboard power for the DBC
	// update and resumes suspending once the lifecycle completes — so gating the
	// DBC's commit on it would fail closed on a picture unrelated to the image
	// under test. The DBC does not read it at all.
	if u.config.Component == "mdb" {
		powerState, powerErr := u.gateProbePowerManagerState()
		powerDetail := fmt.Sprintf("power-manager state %q", powerState)
		if powerErr != nil {
			powerDetail = fmt.Sprintf("cannot read power-manager state: %v", powerErr)
		}
		results = append(results, gateProbeResult{
			Name: gateProbePowerManager, Passed: powerErr == nil && powerState == "running", Detail: powerDetail,
		})
	}

	return append(results, gateProbeResult{
		Name: gateProbeOTAStatus, Passed: otaStatus, Detail: otaDetail,
	})
}

// gateProbeUptime reads time since boot from /proc/uptime. This is monotonic and
// independent of the wall clock, which has no trustworthy source this early in a
// boot.
func (u *Updater) gateProbeUptime() (time.Duration, error) {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, fmt.Errorf("/proc/uptime is empty")
	}
	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("parse /proc/uptime: %w", err)
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

// systemdRunning accepts the states in which boot has finished. "degraded" is
// one of them: whether the failed units matter is the required-unit probe's
// decision, and a unit that is legitimately optional must not block the gate.
func systemdRunning(state string) bool {
	return state == "running" || state == "degraded"
}

func (u *Updater) gateProbeSystemdState() (string, error) {
	// is-system-running exits non-zero while the system is still starting, so
	// the printed state is read regardless of the exit status.
	out, err := exec.Command("systemctl", "is-system-running").Output()
	state := strings.TrimSpace(string(out))
	if state == "" {
		if err != nil {
			return "", fmt.Errorf("systemctl is-system-running: %w", err)
		}
		return "", fmt.Errorf("systemctl is-system-running printed nothing")
	}
	return state, nil
}

// gateProbeUnits returns the first required unit that is neither active nor a
// oneshot that ran successfully during this boot.
//
// is-active alone is not enough: a Type=oneshot unit with RemainAfterExit=no
// reports inactive the instant it has done its job, so a list checked only for
// "active" would reject every window on a healthy system. InvocationID is what
// separates "ran this boot" from "never ran", because Result alone does not:
// systemd reports Result=success for a unit that has never been invoked.
func (u *Updater) gateProbeUnits(units []string) (string, error) {
	for _, unit := range units {
		err := exec.Command("systemctl", "is-active", "--quiet", unit).Run()
		if err == nil {
			continue
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return "", fmt.Errorf("read state of %s: %w", unit, err)
		}
		ran, err := unitRanSuccessfully(unit)
		if err != nil {
			return "", err
		}
		if !ran {
			return unit, nil
		}
	}
	return "", nil
}

// unitRanSuccessfully reports whether a unit has been invoked during this boot
// and ended with a zero exit status.
func unitRanSuccessfully(unit string) (bool, error) {
	out, err := exec.Command("systemctl", "show", unit,
		"-p", "InvocationID", "-p", "Result", "-p", "ExecMainStatus").Output()
	if err != nil {
		return false, fmt.Errorf("show %s: %w", unit, err)
	}
	return unitRanState(string(out)), nil
}

// unitRanState judges a `systemctl show` dump for those three properties.
//
// InvocationID is the discriminator: it is set for a unit that has been invoked
// in this boot and stays set after a oneshot exits, while a unit that never ran
// has none. Result must also be success with a zero exit status, so a failed or
// killed invocation does not count as "ran".
func unitRanState(output string) bool {
	props := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			props[key] = value
		}
	}
	return props["InvocationID"] != "" && props["Result"] == "success" && props["ExecMainStatus"] == "0"
}

// gateProbeVehicleState reports whether vehicle-service has published a state.
// The unit being active is what proves the service is running; this proves it
// reached the point of driving the vehicle state machine.
func (u *Updater) gateProbeVehicleState() (string, error) {
	return u.redis.GetVehicleState(config.VehicleHashKey)
}

// gateProbePowerManagerState reads pm-service's power state. Only running is
// accepted: every other state is a transition, and a commit earned during one is
// a commit about a system that is not the one that keeps running. A missing
// field is a failure, not a pass: it means pm-service has not published a state
// at all.
func (u *Updater) gateProbePowerManagerState() (string, error) {
	return u.redis.HGet("power-manager", "state")
}

// gateProbeOTAStatus requires the component to still be holding its image for
// the gate: pending-reboot with no error recorded against it. A concurrent
// command that failed moves the status and fails this probe.
func (u *Updater) gateProbeOTAStatus() (bool, string) {
	ota, err := u.redis.GetOTAStatus("ota")
	if err != nil {
		return false, fmt.Sprintf("cannot read ota status: %v", err)
	}
	componentStatus := ota[fmt.Sprintf("status:%s", u.config.Component)]
	componentError := ota[fmt.Sprintf("error:%s", u.config.Component)]
	if componentError != "" {
		return false, fmt.Sprintf("component error %q is set", componentError)
	}
	if componentStatus != "pending-reboot" {
		return false, fmt.Sprintf("status is %q, not pending-reboot", componentStatus)
	}
	return true, "status is pending-reboot with no error"
}

func probePassMap(probes []gateProbeResult) map[string]bool {
	passes := make(map[string]bool, len(probes))
	for _, probe := range probes {
		passes[probe.Name] = probe.Passed
	}
	return passes
}

// probePassesEqual reports whether two probe pass maps hold the same names with
// the same outcomes.
func probePassesEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for name, passed := range a {
		if other, ok := b[name]; !ok || other != passed {
			return false
		}
	}
	return true
}

// firstFailingProbe returns the first probe that did not hold, with its detail.
func firstFailingProbe(probes []gateProbeResult) (string, string) {
	for _, probe := range probes {
		if !probe.Passed {
			return probe.Name, probe.Detail
		}
	}
	return "", ""
}
