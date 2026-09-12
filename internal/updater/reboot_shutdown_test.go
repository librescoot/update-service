package updater

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/librescoot/update-service/internal/mender"
)

// The updater must not record a terminal error while it is shutting down: the
// board is rebooting or systemd is stopping the service, and the next instance
// restores the update lifecycle from durable Mender state. Bench evidence
// (2026-09-12): a successful DBC delta update was briefly reported as
// reboot-failed in exactly this window, which made the UMS awaiter record an
// install error and retain MDB reboot ownership.

func TestTriggerDBCRebootWithCanceledContextReturnsWithoutErrorStatus(t *testing.T) {
	u, mr := newPendingCommitUpdater(t)
	u.config.Component = "dbc"
	u.activationAttempt = filepath.Join(t.TempDir(), "activation")
	u.bootID = func() (string, error) { return "boot-a", nil }
	u.observeUpdate = func() (mender.UpdateObservation, error) {
		return mender.UpdateObservation{PendingArtifact: "release-v1.4.0"}, nil
	}
	// Keep the vehicle outside the allowed states so the reboot poll loop runs
	// and hits the canceled context instead of proceeding.
	mr.HSet("vehicle", "state", "running")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	u.ctx = ctx
	if err := u.status.SetPendingReboot(context.Background()); err != nil {
		t.Fatal(err)
	}
	rebooted := false
	u.localReboot = func() error {
		rebooted = true
		return nil
	}

	err := u.TriggerReboot("dbc", true)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("TriggerReboot = %v, want context.Canceled", err)
	}
	if rebooted {
		t.Error("local reboot was requested; the poll loop must exit on shutdown first")
	}

	// The caller path must not turn the shutdown cancellation into a terminal
	// error: the lifecycle stays pending-reboot for the next instance.
	u.setRebootTriggerError("dbc", err)
	got, gerr := u.status.GetStatus(context.Background())
	if gerr != nil {
		t.Fatal(gerr)
	}
	if got != "pending-reboot" {
		t.Fatalf("status after shutdown-triggered reboot error = %q, want pending-reboot", got)
	}
}

func TestSetRebootTriggerErrorSuppressesOnlyOnShutdown(t *testing.T) {
	u, _ := newPendingCommitUpdater(t)
	u.config.Component = "dbc"

	// A genuine failure with a live context must still be reported.
	u.setRebootTriggerError("dbc", errors.New("boom"))
	got, err := u.status.GetStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "error" {
		t.Fatalf("status after live reboot trigger failure = %q, want error", got)
	}

	// The same failure during shutdown must not overwrite the lifecycle.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	u.ctx = ctx
	if err := u.status.SetPendingReboot(context.Background()); err != nil {
		t.Fatal(err)
	}
	u.setRebootTriggerError("dbc", errors.New("boom"))
	got, gerr := u.status.GetStatus(context.Background())
	if gerr != nil {
		t.Fatal(gerr)
	}
	if got != "pending-reboot" {
		t.Fatalf("status after shutdown reboot trigger failure = %q, want pending-reboot", got)
	}
}

func TestSkipTerminalErrorOnShutdown(t *testing.T) {
	u, _ := newPendingCommitUpdater(t)
	if u.skipTerminalErrorOnShutdown("test") {
		t.Error("live context must not suppress terminal error reporting")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	u.ctx = ctx
	if !u.skipTerminalErrorOnShutdown("test") {
		t.Error("canceled context must suppress terminal error reporting")
	}
}
