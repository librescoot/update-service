package updater

import (
	"context"
	"errors"
	"fmt"
	"time"

	ipc "github.com/librescoot/redis-ipc"
)

const dbcBootRebootID = "boot-reboot-id:dbc"

func (u *Updater) currentBootIdentity() (string, error) {
	if u.bootID == nil {
		return "", errors.New("boot identity unavailable")
	}
	id, err := u.bootID()
	if err != nil {
		return "", fmt.Errorf("read boot identity: %w", err)
	}
	if id == "" {
		return "", errors.New("boot identity unavailable")
	}
	return id, nil
}

// The identity is retained until completion is queued. If the process stops
// after publishing idle, the next startup can finish the lifecycle handoff.
func (u *Updater) finishDBCBootCompletion() error {
	raw := u.redis.GetClient().Raw()
	previous, err := raw.HGet(u.ctx, "ota", dbcBootRebootID).Result()
	if errors.Is(err, ipc.ErrNil) || (err == nil && previous == "") {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read boot reboot identity: %w", err)
	}
	id, err := u.currentBootIdentity()
	if err != nil {
		return err
	}
	if previous == id {
		return errors.New("boot reboot identity has not changed")
	}
	if err := u.redis.PushUpdateCommand("complete-dbc"); err != nil {
		return fmt.Errorf("complete DBC boot lifecycle: %w", err)
	}
	if err := raw.HDel(u.ctx, "ota", dbcBootRebootID).Err(); err != nil {
		return fmt.Errorf("clear boot reboot identity: %w", err)
	}
	return nil
}

func (u *Updater) recordDBCBootIdentity(ctx context.Context) error {
	id, err := u.currentBootIdentity()
	if err != nil {
		return err
	}
	return u.redis.GetClient().Raw().HSet(ctx, "ota", dbcBootRebootID, id).Err()
}

// updateOpMu is held until the reboot or cancellation, preventing a rootfs
// operation from being interrupted by the pending boot reboot.
func (u *Updater) startDBCBootRebootWait(ctx context.Context, cancel context.CancelFunc, stop func() error) {
	u.wg.Add(1)
	go func() {
		defer u.wg.Done()
		defer u.updateOpMu.Unlock()
		defer cancel()
		defer func() {
			if err := stop(); err != nil {
				u.logger.Printf("[boot-local] heartbeat stopped: %v", err)
			}
		}()
		u.waitDBCBootReboot(ctx)
	}()
}

func (u *Updater) waitDBCBootReboot(ctx context.Context) {
	if u.config.DryRun {
		u.logger.Printf("[boot-local] dry-run: DBC reboot remains pending")
		return
	}
	for ctx.Err() == nil {
		if err := u.triggerDBCLocalRebootContext(ctx, false); err != nil {
			u.logger.Printf("[boot-local] DBC reboot request failed: %v", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
				continue
			}
		}
		// Acceptance is not shutdown. Keep the heartbeat alive and retry if
		// the accepted systemd job does not stop this process.
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
}

// Re-establish the lifecycle after a same-boot service restart or a legacy
// pending status. The status remains nonterminal on every failure.
func (u *Updater) resumeDBCBootReboot() {
	if !u.updateOpMu.TryLock() {
		return
	}
	u.wg.Add(1)
	go func() {
		defer u.wg.Done()
		defer u.updateOpMu.Unlock()
		ctx, cancel := context.WithCancel(u.ctx)
		defer cancel()
		raw := u.redis.GetClient().Raw()
		active, err := raw.HGet(ctx, "vehicle", "dbc-updating").Result()
		if err != nil && !errors.Is(err, ipc.ErrNil) {
			u.logger.Printf("[boot-local] cannot read DBC lifecycle: %v", err)
			return
		}
		if active != "true" {
			if active != "false" && active != "" {
				u.logger.Printf("[boot-local] unknown DBC lifecycle state %q", active)
				return
			}
			if err := raw.LPush(ctx, "scooter:update", "start-dbc").Err(); err != nil {
				u.logger.Printf("[boot-local] cannot restore DBC lifecycle: %v", err)
				return
			}
			if err := waitBootDBC(ctx, bootGuardTimeout, 100*time.Millisecond, func(ctx context.Context) (bool, error) {
				value, err := raw.HGet(ctx, "vehicle", "dbc-updating").Result()
				return value == "true", err
			}); err != nil {
				u.logger.Printf("[boot-local] cannot confirm DBC lifecycle: %v", err)
				return
			}
		}
		stop, err := startBootHeartbeat(ctx, u.status, heartbeatInterval, cancel)
		if err != nil {
			u.logger.Printf("[boot-local] cannot restore DBC heartbeat: %v", err)
			return
		}
		defer func() { _ = stop() }()
		u.waitDBCBootReboot(ctx)
	}()
}
