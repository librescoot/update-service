package updater

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/librescoot/update-service/internal/catalog"
	"github.com/librescoot/update-service/internal/mender"
)

func (u *Updater) executeV2(p catalog.Plan, manual bool) {
	if !u.updateOpMu.TryLock() {
		u.logger.Printf("Another update is active")
		return
	}
	defer u.updateOpMu.Unlock()
	defer u.startHeartbeat()()
	if u.gateWindowIsOpen() {
		u.logger.Printf("Commit gate is active")
		return
	}
	obs, err := u.observeUpdate()
	if err != nil || obs.PendingArtifact != "" || (obs.State != mender.StateNoUpdate && obs.State != mender.StateCommitted) {
		u.logger.Printf("Mender pending or unavailable: %v", err)
		return
	}
	dir := u.mender.GetDownloadDir()
	if existing, readErr := loadV2Plan(dir); readErr == nil {
		if existing.Target.ID != p.Target.ID || existing.InstallStarted {
			u.logger.Printf("Refusing to replace an active or unconfirmed v2 plan")
			return
		}
	} else if !os.IsNotExist(readErr) {
		u.logger.Printf("Refusing to overwrite invalid v2 plan: %v", readErr)
		return
	}
	if err = saveV2Plan(dir, p); err != nil {
		u.logger.Printf("Cannot persist v2 plan: %v", err)
		return
	}
	u.executeV2Locked(p, manual)
}

func (u *Updater) executeV2Locked(p catalog.Plan, manual bool) {
	dir := u.mender.GetDownloadDir()
	if p.Method() == "full" {
		u.v2FullFallback(p, manual)
		return
	}
	basePath, ok := u.mender.FindMenderFileForVersion(p.Base.Version)
	if !ok {
		u.v2FullFallback(p, manual)
		return
	}
	if !enoughDeltaSpace(p, dir) {
		u.logger.Printf("Insufficient scratch space for delta; using full")
		u.v2FullFallback(p, manual)
		return
	}
	if err := catalog.Verify(u.ctx, basePath, p.Base); err != nil {
		if u.ctx.Err() != nil {
			return
		}
		u.logger.Printf("Delta base verification: %v", err)
		u.v2FullFallback(p, manual)
		return
	}
	targetName := "librescoot-" + p.Target.Variant + "-" + p.Target.Version + ".mender"
	targetPath := filepath.Join(dir, targetName)
	if err := u.status.SetDownloading(u.ctx, strings.ToLower(p.Target.Version), "delta"); err != nil {
		return
	}
	u.finishCheck(checkResultUpdateStarted)
	if u.config.Component == "dbc" {
		if err := u.redis.PushUpdateCommand("start-dbc"); err != nil {
			u.logger.Printf("DBC start: %v", err)
		}
		defer u.completeDBCLifecycleIfSafe()
	}
	if u.config.Component == "mdb" {
		if err := u.inhibitor.AddDownloadInhibit(u.config.Component); err != nil {
			u.logger.Printf("download inhibit: %v", err)
		}
		defer u.inhibitor.RemoveDownloadInhibit(u.config.Component)
		defer u.inhibitor.RemovePreparingInhibit(u.config.Component)
	}
	if err := u.power.RequestOndemandGovernor(); err != nil {
		u.logger.Printf("CPU governor: %v", err)
	}
	defer u.holdSuspend()()
	// Reuse an interrupted reconstruction only within the normal power lifecycle.
	if _, err := os.Stat(targetPath); err == nil && catalog.Verify(u.ctx, targetPath, p.Target) == nil {
		u.v2Install(p, targetPath, manual)
		return
	}
	var files []string
	for _, step := range p.Steps {
		deadline := time.Now().Add(deltaDownloadMaxRetryDuration)
		checksumFailures := 0
		for {
			var progress progressTracker
			file, err := u.mender.DownloadDelta(u.ctx, step.URL, "sha256:"+step.SHA256, func(done, total int64) {
				progress.observe(done)
				_ = u.status.SetDownloadProgress(u.ctx, done, total)
			})
			if err == nil {
				files = append(files, file)
				break
			}
			if u.ctx.Err() != nil {
				return
			}
			if isDownloadBudgetAbort(err) {
				u.recordDownloadAbort(p.Target.Version, err, progress.gained())
				return
			}
			if errors.Is(err, mender.ErrChecksumMismatch) {
				checksumFailures++
			}
			if errors.Is(err, mender.ErrAssetUnavailable) || checksumFailures >= deltaChecksumFailureLimit || !time.Now().Before(deadline) {
				u.logger.Printf("Delta download failed: %v", err)
				u.v2FullFallback(p, manual)
				return
			}
			wait := min(time.Until(deadline), deltaDownloadRetryInterval)
			select {
			case <-u.ctx.Done():
				return
			case <-time.After(wait):
			}
		}
	}
	if err := verifyV2Deltas(p, files); err != nil {
		u.logger.Printf("Delta metadata mismatch: %v", err)
		u.v2FullFallback(p, manual)
		return
	}
	if err := u.status.SetPreparing(u.ctx); err != nil {
		return
	}
	if u.config.Component == "mdb" {
		if err := u.inhibitor.AddPreparingInhibit(u.config.Component); err != nil {
			u.logger.Printf("preparing inhibit: %v", err)
		}
	}
	out, err := u.mender.ApplyExplicitDeltaChain(u.ctx, basePath, files, targetName, func(pc int) { _ = u.status.SetInstallProgress(u.ctx, pc) })
	if err != nil {
		if u.ctx.Err() != nil {
			return
		}
		u.logger.Printf("Delta apply failed: %v", err)
		u.v2FullFallback(p, manual)
		return
	}
	if err = catalog.Verify(u.ctx, out, p.Target); err != nil {
		_ = os.Remove(out)
		if u.ctx.Err() != nil {
			return
		}
		u.logger.Printf("Target verification: %v", err)
		u.v2FullFallback(p, manual)
		return
	}
	u.v2Install(p, out, manual)
}

func (u *Updater) v2Install(p catalog.Plan, file string, manual bool) {
	if err := u.backoff.Clear(); err != nil {
		u.logger.Printf("Cannot clear download backoff: %v", err)
	}
	u.revalidateStandbyState()
	if u.config.DryRun {
		u.logger.Printf("DRY-RUN: verified v2 target %s", file)
		_ = u.status.SetIdle(u.ctx)
		return
	}
	if err := u.status.SetInstalling(u.ctx); err != nil {
		u.logger.Printf("Cannot publish v2 install status: %v", err)
		return
	}
	if u.config.Component == "mdb" {
		if err := u.inhibitor.AddInstallInhibit(u.config.Component); err != nil {
			u.logger.Printf("install inhibit: %v", err)
		}
		defer u.inhibitor.RemoveInstallInhibit(u.config.Component)
	}
	p.InstallStarted = true
	if err := saveV2Plan(u.mender.GetDownloadDir(), p); err != nil {
		u.logger.Printf("Cannot persist v2 install intent: %v", err)
		return
	}
	if err := u.installMender(file); err != nil {
		if !u.skipTerminalErrorOnShutdown("v2 install") {
			_ = u.status.SetError(u.ctx, installErrorCode(err), err.Error())
		}
		return
	}
	if err := u.status.SetPendingReboot(u.ctx); err != nil {
		u.logger.Printf("Cannot publish pending reboot: %v", err)
	}
	if err := u.TriggerReboot(u.config.Component, manual); err != nil && !strings.Contains(err.Error(), "DRY-RUN") {
		u.setRebootTriggerError(u.config.Component, err)
	}
}

func (u *Updater) v2FullFallback(p catalog.Plan, manual bool) {
	// Pin the exact target and digest, not a freshly selected latest release.
	release := Release{TagName: p.Target.Version, Assets: []Asset{{Name: filepath.Base(p.Target.Full.URL), URL: p.Target.Full.URL, Size: p.Target.Full.Size, Sha256: p.Target.Full.SHA256}}}
	u.logger.Printf("Using full target %s", p.Target.Version)
	// The legacy full installer owns its own recovery. Do not leave a delta
	// plan which could be retried after that installer rolls back.
	if err := clearV2Plan(u.mender.GetDownloadDir()); err != nil {
		u.logger.Printf("Cannot clear v2 plan before full fallback: %v", err)
		return
	}
	u.performUpdateLocked(release, p.Target.Full.URL, manual)
}

func (u *Updater) finishV2Plan(committedArtifact string) {
	if u.mender == nil {
		return
	}
	p, err := loadV2Plan(u.mender.GetDownloadDir())
	if err != nil || p.Target.Name != committedArtifact {
		return
	}
	if err := clearV2Plan(u.mender.GetDownloadDir()); err != nil {
		u.logger.Printf("Cannot clear committed v2 plan: %v", err)
	}
}

// cancelV2Plan is an explicit recovery action, never a Mender rollback.
func (u *Updater) cancelV2Plan() {
	if !u.updateOpMu.TryLock() {
		u.logger.Printf("Cannot cancel v2 plan while an update is running")
		return
	}
	defer u.updateOpMu.Unlock()
	if u.gateWindowIsOpen() {
		return
	}
	obs, err := u.observeUpdate()
	if err != nil || obs.PendingArtifact != "" || (obs.State != mender.StateNoUpdate && obs.State != mender.StateCommitted) {
		u.logger.Printf("Cannot cancel v2 plan while Mender is pending: %v", err)
		return
	}
	if err := clearV2Plan(u.mender.GetDownloadDir()); err != nil {
		u.logger.Printf("Cannot cancel v2 plan: %v", err)
		return
	}
	if err := u.status.SetIdle(u.ctx); err != nil {
		u.logger.Printf("Cannot clear v2 status: %v", err)
	}
}

// recoverV2Plan never installs while Mender still holds a pending update.
func (u *Updater) recoverV2Plan(holdsPending bool) bool {
	p, err := loadV2Plan(u.mender.GetDownloadDir())
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		u.logger.Printf("Invalid saved OTA plan: %v", err)
		return true
	}
	running, err := u.runningVersion()
	if err == nil && strings.EqualFold(running, p.Target.Version) {
		if !holdsPending {
			if err := clearV2Plan(u.mender.GetDownloadDir()); err != nil {
				u.logger.Printf("clear OTA plan: %v", err)
			}
		}
		return holdsPending
	}
	if holdsPending {
		return true
	}
	if p.InstallStarted {
		u.logger.Printf("OTA target %s was not committed after installation; refusing automatic retry", p.Target.Version)
		_ = u.status.SetError(u.ctx, "v2-install-unconfirmed", "previous update was not committed; manual intervention required")
		return true
	}
	u.wg.Add(1)
	go func() { defer u.wg.Done(); u.executeV2(p, false) }()
	return true
}
