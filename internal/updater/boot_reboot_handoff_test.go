package updater

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func waitSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}
}

func TestResumeDBCBootRebootLifecycle(t *testing.T) {
	for _, mode := range []string{"accepted", "failed", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			u, mr := bootRecoveryFixture(t, "dbc")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			u.ctx = ctx
			mr.HSet("ota", "status:dbc", "idle", "status:dbc-boot", "pending-reboot", dbcBootRebootID, "current-boot")
			state := "stand-by"
			if mode == "cancelled" {
				state = "ready-to-drive"
			}
			mr.HSet("vehicle", "state", state, "dbc-updating", "true", "dashboard:power", "on")
			called := make(chan struct{}, 1)
			u.localReboot = func() error {
				called <- struct{}{}
				if mode == "failed" {
					return errors.New("reboot rejected")
				}
				return nil
			}
			u.resumeDBCBootReboot()
			if mode != "cancelled" {
				waitSignal(t, called)
			}
			// Both accepted and failed requests retain the operation until
			// shutdown or a retry; neither is evidence of a completed reboot.
			if mode != "cancelled" && u.updateOpMu.TryLock() {
				u.updateOpMu.Unlock()
				t.Fatal("operation lock released before shutdown")
			}
			cancel()
			done := make(chan struct{})
			go func() { u.wg.Wait(); close(done) }()
			waitSignal(t, done)
			if mode == "cancelled" {
				select {
				case <-called:
					t.Fatal("rebooted outside standby")
				default:
				}
			}
			if got := mr.HGet("ota", "status:dbc-boot"); got != "pending-reboot" {
				t.Fatalf("%s status=%q", mode, got)
			}
			if mr.Exists("scooter:update") {
				t.Fatal("lifecycle completed without a verified reboot")
			}
			stateAfter, err := u.redis.GetMDBRebootState(context.Background())
			if err != nil || mdbRebootWaitReason(stateAfter) == "" {
				t.Fatalf("MDB not blocked: %+v, %v", stateAfter, err)
			}
		})
	}
}

func TestDBCBootRetriesRejectedReboot(t *testing.T) {
	u, mr := bootRecoveryFixture(t, "dbc")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	u.ctx = ctx
	mr.HSet("ota", "status:dbc", "idle", "status:dbc-boot", "pending-reboot", dbcBootRebootID, "current-boot")
	mr.HSet("vehicle", "state", "stand-by", "dbc-updating", "true", "dashboard:power", "on")
	var attempts atomic.Int32
	second := make(chan struct{})
	u.localReboot = func() error {
		if attempts.Add(1) == 2 {
			close(second)
			return nil
		}
		return errors.New("reboot rejected")
	}
	u.resumeDBCBootReboot()
	select {
	case <-second:
	case <-time.After(8 * time.Second):
		t.Fatal("rejected reboot was not retried")
	}
	if mr.HGet("ota", "status:dbc-boot") != "pending-reboot" || mr.Exists("scooter:update") {
		t.Fatal("retry falsely completed lifecycle")
	}
	cancel()
	done := make(chan struct{})
	go func() { u.wg.Wait(); close(done) }()
	waitSignal(t, done)
}

type failingHeartbeat struct{ calls atomic.Int32 }

func (h *failingHeartbeat) SetHeartbeat(context.Context, time.Time) error {
	if h.calls.Add(1) > 1 {
		return errors.New("heartbeat failure")
	}
	return nil
}
func (h *failingHeartbeat) ClearHeartbeat(context.Context) error { return nil }

func TestBootHeartbeatFailureCancelsWaitWithoutCompletion(t *testing.T) {
	u, mr := bootRecoveryFixture(t, "dbc")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	u.ctx = ctx
	mr.HSet("ota", "status:dbc", "idle", "status:dbc-boot", "pending-reboot", dbcBootRebootID, "current-boot")
	mr.HSet("vehicle", "state", "ready-to-drive", "dbc-updating", "true", "dashboard:power", "on")
	waitCtx, waitCancel := context.WithCancel(ctx)
	h := &failingHeartbeat{}
	stop, err := startBootHeartbeat(waitCtx, h, time.Millisecond, waitCancel)
	if err != nil {
		t.Fatal(err)
	}
	u.updateOpMu.Lock()
	u.localReboot = func() error { t.Error("rebooted after heartbeat failure"); return nil }
	u.startDBCBootRebootWait(waitCtx, waitCancel, stop)
	done := make(chan struct{})
	go func() { u.wg.Wait(); close(done) }()
	waitSignal(t, done)
	if mr.HGet("ota", "status:dbc-boot") != "pending-reboot" || mr.Exists("scooter:update") {
		t.Fatal("heartbeat failure falsely completed boot")
	}
	state, err := u.redis.GetMDBRebootState(ctx)
	if err != nil || mdbRebootWaitReason(state) == "" {
		t.Fatalf("MDB not blocked: %+v, %v", state, err)
	}
}
