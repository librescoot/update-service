package updater

import (
	"context"
	"testing"
	"time"
)

func TestResumeDBCBootAfterWatchdog(t *testing.T) {
	u, mr := bootRecoveryFixture(t, "dbc")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	u.ctx = ctx
	mr.HSet("ota", "status:dbc", "idle", "status:dbc-boot", "pending-reboot", dbcBootRebootID, "current-boot")
	mr.HSet("vehicle", "state", "stand-by", "dbc-updating", "false", "dashboard:power", "on")
	called := make(chan struct{}, 1)
	u.localReboot = func() error { called <- struct{}{}; return nil }
	u.resumeDBCBootReboot()
	deadline := time.Now().Add(2 * time.Second)
	for !mr.Exists("scooter:update") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	commands, err := mr.List("scooter:update")
	if err != nil || len(commands) != 1 || commands[0] != "start-dbc" {
		t.Fatalf("commands=%v err=%v", commands, err)
	}
	select {
	case <-called:
		t.Fatal("reboot before vehicle confirmed lifecycle")
	default:
	}
	mr.HSet("vehicle", "dbc-updating", "true")
	waitSignal(t, called)
	if mr.HGet("ota", "status:dbc-boot") != "pending-reboot" {
		t.Fatal("reboot request cleared status")
	}
	cancel()
	done := make(chan struct{})
	go func() { u.wg.Wait(); close(done) }()
	waitSignal(t, done)
}

func TestDBCBootCompletionResumesAfterIdleWrite(t *testing.T) {
	for _, previous := range []string{"previous-boot", "current-boot"} {
		t.Run(previous, func(t *testing.T) {
			u, mr := bootRecoveryFixture(t, "dbc")
			mr.HSet("ota", "status:dbc", "idle", "status:dbc-boot", "idle", dbcBootRebootID, previous)
			err := u.recoverFromStuckState(false, false)
			if previous == "current-boot" {
				if err == nil || mr.Exists("scooter:update") {
					t.Fatal("same-boot identity completed lifecycle")
				}
				return
			}
			commands, listErr := mr.List("scooter:update")
			if err != nil || listErr != nil || len(commands) != 1 || commands[0] != "complete-dbc" || mr.HGet("ota", dbcBootRebootID) != "" {
				t.Fatalf("commands=%v recover=%v list=%v", commands, err, listErr)
			}
		})
	}
}

func TestRootfsCompletionDoesNotReleaseBootLifecycle(t *testing.T) {
	for _, bootStatus := range []string{"pending-reboot", "unknown", "installing"} {
		t.Run(bootStatus, func(t *testing.T) {
			u, mr := bootRecoveryFixture(t, "dbc")
			mr.HSet("ota", "status:dbc", "pending-reboot", "status:dbc-boot", bootStatus, dbcBootRebootID, "current-boot")
			if err := u.recoverFromStuckState(false, false); err != nil {
				t.Fatal(err)
			}
			if mr.Exists("scooter:update") {
				t.Fatal("rootfs recovery completed an active or unknown boot lifecycle")
			}
			if got := mr.HGet("ota", "status:dbc"); got != "idle" {
				t.Fatalf("rootfs status=%q", got)
			}
		})
	}
}
