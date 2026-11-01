package updater

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type countingHeartbeat struct{ ticks atomic.Int32 }

func (h *countingHeartbeat) SetHeartbeat(context.Context, time.Time) error {
	h.ticks.Add(1)
	return nil
}
func (h *countingHeartbeat) ClearHeartbeat(context.Context) error { return nil }

// The command callback models vehicle-service consuming the lifecycle FIFO in
// standby, where completion can initiate dashboard power-off.
func TestDBCBootLifecycleSurvivesUntilReboot(t *testing.T) {
	u, mr := bootRecoveryFixture(t, "dbc")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	u.ctx = ctx
	id := "boot-a"
	u.bootID = func() (string, error) { return id, nil }
	mr.HSet("vehicle", "state", "stand-by", "dashboard:power", "on", "dbc-updating", "false")
	mr.HSet("ota", "status:dbc", "idle")
	f := newFakeBoot()
	d := f.deps()
	d.status = u.bootStatus
	h := &countingHeartbeat{}
	d.heartbeat, d.heartbeatInterval = h, time.Millisecond
	d.dbcUpdating = func(context.Context) (bool, error) { return mr.HGet("vehicle", "dbc-updating") == "true", nil }
	d.command = func(ctx context.Context, cmd string) error {
		if err := u.redis.GetClient().Raw().LPush(ctx, "scooter:update", cmd).Err(); err != nil {
			return err
		}
		switch cmd {
		case "start-dbc":
			mr.HSet("vehicle", "dbc-updating", "true")
		case "complete-dbc":
			mr.HSet("vehicle", "dbc-updating", "false", "dashboard:power", "off")
		}
		return nil
	}
	d.prepareReboot = u.recordDBCBootIdentity
	var waitCtx context.Context
	var waitCancel context.CancelFunc
	var stop func() error
	d.handoff = func(ctx context.Context, cancel context.CancelFunc, done func() error) {
		waitCtx, waitCancel, stop = ctx, cancel, done
	}
	applied, err := runLocalBootUpdate(ctx, "dbc", d)
	if err != nil || !applied {
		t.Fatalf("install: applied=%v err=%v", applied, err)
	}
	if f.held || mr.HGet("vehicle", "dashboard:power") != "on" || mr.HGet("vehicle", "dbc-updating") != "true" {
		t.Fatal("flash block not released or lifecycle lost before reboot")
	}
	if got := mr.HGet("ota", "status:dbc-boot"); got != "pending-reboot" {
		t.Fatalf("boot status=%q", got)
	}
	state, err := u.redis.GetMDBRebootState(ctx)
	if err != nil || mdbRebootWaitReason(state) == "" {
		t.Fatalf("MDB guard: state=%+v err=%v", state, err)
	}
	time.Sleep(10 * time.Millisecond)
	if h.ticks.Load() < 2 {
		t.Fatal("heartbeat did not continue after flash")
	}
	if err := u.recoverFromStuckState(false, false); err != nil {
		t.Fatal(err)
	}
	if mr.HGet("ota", "status:dbc-boot") != "pending-reboot" {
		t.Fatal("same-boot restart falsely completed reboot")
	}
	if waitCtx.Err() != nil {
		t.Fatal("heartbeat context cancelled after handoff")
	}
	waitCancel()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	id = "boot-b"
	if err := u.recoverFromStuckState(false, false); err != nil {
		t.Fatal(err)
	}
	if mr.HGet("ota", "status:dbc-boot") != "idle" {
		t.Fatal("completed reboot not recovered")
	}
	commands, err := mr.List("scooter:update")
	if err != nil || len(commands) != 2 || commands[0] != "complete-dbc" || commands[1] != "start-dbc" {
		t.Fatalf("commands=%v err=%v", commands, err)
	}
	state, err = u.redis.GetMDBRebootState(ctx)
	if err != nil || mdbRebootWaitReason(state) == "" {
		t.Fatal("MDB bypassed lifecycle and power-off gates")
	}
	mr.HSet("vehicle", "dbc-updating", "false", "dashboard:power", "off")
	state, err = u.redis.GetMDBRebootState(ctx)
	if err != nil || mdbRebootWaitReason(state) != "" {
		t.Fatalf("MDB blocked after completed lifecycle: %+v %v", state, err)
	}
}

func TestDBCBootLegacyAndUnknownIdentityStayPending(t *testing.T) {
	u, mr := bootRecoveryFixture(t, "dbc")
	mr.HSet("ota", "status:dbc", "idle", "status:dbc-boot", "pending-reboot")
	if err := u.recoverFromStuckState(false, false); err != nil {
		t.Fatal(err)
	}
	if mr.HGet("ota", "status:dbc-boot") != "pending-reboot" || mr.HGet("ota", dbcBootRebootID) != "current-boot" {
		t.Fatal("legacy pending status was treated as completed")
	}
	u.bootID = func() (string, error) { return "", errors.New("unavailable") }
	if err := u.recoverFromStuckState(false, false); err == nil {
		t.Fatal("unknown boot identity accepted")
	}
	if mr.HGet("ota", "status:dbc-boot") != "pending-reboot" {
		t.Fatal("unknown identity cleared pending status")
	}
}
