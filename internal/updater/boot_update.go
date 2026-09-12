package updater

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	ipc "github.com/librescoot/redis-ipc"
	"github.com/librescoot/update-service/internal/boot"
	"github.com/librescoot/update-service/internal/mender"
)

func bootComponent(component string) string { return component + "-boot" }

// Called at startup before any no-op boot checks. This releases an orphaned
// process hold; it cannot repair a bootloader interrupted by a previous crash.
func cleanupBootUpdate(guard bootInstallGuard, component string) error {
	return guard.RemoveBootInstallInhibit(component)
}

type bootWriter interface {
	UpToDate(string) (bool, error)
	Apply(context.Context, string) error
}

type bootInstallGuard interface {
	AddBootInstallInhibit(string, string) error
	RemoveBootInstallInhibit(string) error
}

type bootReporter interface {
	SetInstalling(context.Context) error
	SetPendingReboot(context.Context) error
	SetError(context.Context, string, string) error
}

type bootHeartbeat interface {
	SetHeartbeat(context.Context, time.Time) error
	ClearHeartbeat(context.Context) error
}

// Keep the destructive operation testable without device access or a live vehicle.
type bootUpdateDeps struct {
	writer            bootWriter
	guard             bootInstallGuard
	status            bootReporter
	heartbeat         bootHeartbeat
	rootfsState       func() (mender.UpdateState, error)
	command           func(context.Context, string) error
	dbcUpdating       func(context.Context) (bool, error)
	powerObserved     func(context.Context, string) (bool, error)
	ackTimeout        time.Duration
	pollInterval      time.Duration
	heartbeatInterval time.Duration
}

const bootGuardTimeout = 10 * time.Second

func waitBootDBC(ctx context.Context, timeout, interval time.Duration, read func(context.Context) (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ready, err := read(ctx)
		if err != nil {
			return err
		}
		if ready {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// The heartbeat outlives caller cancellation until Apply has returned: cancellation
// is not evidence that the writer has stopped touching the boot region.
func startBootHeartbeat(ctx context.Context, reporter bootHeartbeat, interval time.Duration, cancelWrite context.CancelFunc) (func() error, error) {
	base := context.WithoutCancel(ctx)
	tick := func() error {
		c, cancel := context.WithTimeout(base, bootGuardTimeout)
		defer cancel()
		return reporter.SetHeartbeat(c, time.Now())
	}
	if err := tick(); err != nil {
		return nil, err
	}
	stop, done := make(chan struct{}), make(chan struct{})
	var heartbeatErr error
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if err := tick(); err != nil {
					heartbeatErr = err
					cancelWrite()
					return
				}
			}
		}
	}()
	return func() error {
		close(stop)
		<-done
		c, cancel := context.WithTimeout(base, bootGuardTimeout)
		defer cancel()
		return errors.Join(heartbeatErr, reporter.ClearHeartbeat(c))
	}, nil
}

// Caller holds updateOpMu. Only a fully settled rootfs may supply boot assets;
// a pending rootfs could otherwise reboot into an image we did not compare.
func runLocalBootUpdate(ctx context.Context, component string, d bootUpdateDeps) (applied bool, result error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	state, err := d.rootfsState()
	if err != nil {
		return false, fmt.Errorf("check rootfs state: %w", err)
	}
	if state != mender.StateNoUpdate && state != mender.StateCommitted {
		return false, fmt.Errorf("rootfs update is not settled: %v", state)
	}
	current, err := d.writer.UpToDate(boot.LocalAssetsPath)
	if err != nil {
		return false, err
	}
	if current {
		return false, nil
	}
	if d.ackTimeout <= 0 {
		d.ackTimeout = bootGuardTimeout
	}
	if d.pollInterval <= 0 {
		d.pollInterval = 100 * time.Millisecond
	}
	if d.heartbeatInterval <= 0 {
		d.heartbeatInterval = heartbeatInterval
	}
	ctx, cancelWrite := context.WithCancel(ctx)
	defer cancelWrite()
	cleanupCtx := context.WithoutCancel(ctx)
	defer func() {
		if result != nil && !errors.Is(result, context.Canceled) {
			c, cancel := context.WithTimeout(cleanupCtx, bootGuardTimeout)
			defer cancel()
			result = errors.Join(result, d.status.SetError(c, "install-failed", result.Error()))
		}
	}()
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return false, fmt.Errorf("create boot request ID: %w", err)
	}
	requestID := fmt.Sprintf("%x", token)
	// Even an error reply can follow an executed Redis transaction. Attempt to
	// remove our own key, but never proceed to a write on an ambiguous acquisition.
	defer func() { result = errors.Join(result, d.guard.RemoveBootInstallInhibit(component)) }()
	if err := d.guard.AddBootInstallInhibit(component, requestID); err != nil {
		return false, fmt.Errorf("acquire boot power block: %w", err)
	}
	if err := waitBootDBC(ctx, d.ackTimeout, d.pollInterval, func(ctx context.Context) (bool, error) {
		return d.powerObserved(ctx, requestID)
	}); err != nil {
		return false, fmt.Errorf("await processed boot power block: %w", err)
	}
	if err := d.status.SetInstalling(ctx); err != nil {
		return false, fmt.Errorf("publish boot installing: %w", err)
	}
	if component == "dbc" {
		// FIFO commands ensure even a timed-out start request is followed by cleanup.
		defer func() {
			c, cancel := context.WithTimeout(cleanupCtx, bootGuardTimeout)
			defer cancel()
			result = errors.Join(result, d.command(c, "complete-dbc"))
		}()
		c, cancel := context.WithTimeout(ctx, d.ackTimeout)
		err := d.command(c, "start-dbc")
		cancel()
		if err != nil {
			return false, fmt.Errorf("start DBC lifecycle: %w", err)
		}
		if err := waitBootDBC(ctx, d.ackTimeout, d.pollInterval, d.dbcUpdating); err != nil {
			return false, fmt.Errorf("await DBC power protection: %w", err)
		}
		stopHeartbeat, err := startBootHeartbeat(ctx, d.heartbeat, d.heartbeatInterval, cancelWrite)
		if err != nil {
			return false, fmt.Errorf("start DBC heartbeat: %w", err)
		}
		defer func() { result = errors.Join(result, stopHeartbeat()) }()
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	checkCtx, cancelCheck := context.WithTimeout(ctx, d.ackTimeout)
	observed, err := d.powerObserved(checkCtx, requestID)
	cancelCheck()
	if err != nil {
		return false, fmt.Errorf("recheck boot power block: %w", err)
	}
	if !observed {
		return false, fmt.Errorf("processed boot power block disappeared")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := d.writer.Apply(ctx, boot.LocalAssetsPath); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := d.status.SetPendingReboot(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func (u *Updater) applyLocalBootUpdate(ctx context.Context, component string, d bootUpdateDeps) (bool, error) {
	if !u.updateOpMu.TryLock() {
		return false, fmt.Errorf("another update operation is active")
	}
	defer u.updateOpMu.Unlock()
	return runLocalBootUpdate(ctx, component, d)
}

func (u *Updater) performLocalBootUpdate() {
	if u.bootUpdater == nil || !boot.HasLocalAssets() {
		return
	}
	if !u.updateOpMu.TryLock() {
		u.logger.Printf("[boot-local] another update operation is active")
		return
	}
	unlockOnReturn := true
	defer func() {
		if unlockOnReturn {
			u.updateOpMu.Unlock()
		}
	}()
	raw := u.redis.GetClient().Raw()
	applied, err := runLocalBootUpdate(u.ctx, u.config.Component, bootUpdateDeps{
		writer: u.bootUpdater, guard: u.inhibitor, status: u.bootStatus, heartbeat: u.status,
		rootfsState: func() (mender.UpdateState, error) { return u.mender.CheckUpdateState("") },
		powerObserved: func(ctx context.Context, requestID string) (bool, error) {
			return u.inhibitor.BootInstallObserved(ctx, u.config.Component, requestID)
		},
		command: func(ctx context.Context, command string) error {
			return raw.LPush(ctx, "scooter:update", command).Err()
		},
		dbcUpdating: func(ctx context.Context) (bool, error) {
			value, err := raw.HGet(ctx, "vehicle", "dbc-updating").Result()
			if errors.Is(err, ipc.ErrNil) {
				return false, nil
			}
			return value == "true", err
		},
	})
	if err != nil {
		u.logger.Printf("[boot-local] aborted: %v", err)
		u.recordBootAbort(err)
		return
	}
	if !applied {
		return
	}
	u.logger.Printf("[boot-local] boot update applied, deferring reboot to background")
	// Transfer the operation lock to the reboot waiter without a gap: a
	// rootfs install must not start and then be interrupted by this reboot.
	unlockOnReturn = false
	u.wg.Add(1)
	go func() {
		defer u.wg.Done()
		defer u.updateOpMu.Unlock()
		if err := u.TriggerBootReboot(u.config.Component, true); err != nil {
			if !strings.Contains(err.Error(), "DRY-RUN") {
				u.logger.Printf("[boot-local] reboot trigger failed: %v", err)
				if u.skipTerminalErrorOnShutdown("[boot-local] reboot trigger") {
					return
				}
				if err := u.bootStatus.SetError(u.ctx, "reboot-failed", err.Error()); err != nil {
					u.logger.Printf("[boot-local] failed to set error status: %v", err)
				}
			} else {
				u.logger.Printf("[boot-local] dry-run: simulating post-reboot state")
				if err := u.bootStatus.SetIdle(u.ctx); err != nil {
					u.logger.Printf("[boot-local] failed to set idle status in dry run: %v", err)
				}
			}
		}
	}()
}

// recordBootAbort publishes a boot-update abort. Validation and rootfs-state
// failures can occur before the guarded install begins; publish those too
// rather than retaining stale success. Shutdown-induced cancellations are not
// failures: the board is rebooting or systemd is stopping the service, and the
// next instance restores the update lifecycle, so no terminal status is written.
func (u *Updater) recordBootAbort(err error) {
	if u.skipTerminalErrorOnShutdown("[boot-local] update") {
		return
	}
	statusCtx, cancel := context.WithTimeout(context.WithoutCancel(u.ctx), bootGuardTimeout)
	defer cancel()
	if statusErr := u.bootStatus.SetError(statusCtx, "install-failed", err.Error()); statusErr != nil {
		u.logger.Printf("[boot-local] failed to report abort: %v", statusErr)
	}
}
