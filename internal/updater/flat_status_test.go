package updater

import (
	"context"
	"log"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/librescoot/update-service/internal/redis"
	"github.com/librescoot/update-service/internal/status"
)

// newTestUpdaterForFlatStatus builds an MDB-side Updater with just enough
// wired up to exercise monitorFlatStatus: a real redis.Client and pair of
// status.Reporters backed by miniredis, matching the construction New()
// does under the "mdb" guard.
func newTestUpdaterForFlatStatus(t *testing.T) (*Updater, *status.Reporter, *status.Reporter, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rc, err := redis.New(mr.Addr())
	if err != nil {
		t.Fatalf("connecting test redis client: %v", err)
	}
	t.Cleanup(func() { _ = rc.Close() })

	logger := log.New(os.Stdout, "test: ", 0)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	mdbStatus := status.NewReporter(rc.GetClient(), "mdb", logger)
	dbcStatus := status.NewReporter(rc.GetClient(), "dbc", logger)

	u := &Updater{
		redis:      rc,
		status:     mdbStatus,
		dbcStatus:  dbcStatus,
		flatMirror: status.NewFlatMirror(rc.GetClient(), logger),
		logger:     logger,
		ctx:        ctx,
		cancel:     cancel,
	}
	return u, mdbStatus, dbcStatus, mr
}

func waitForFlatField(t *testing.T, mr *miniredis.Miniredis, field, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		got = mr.HGet("ota", field)
		if got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s = %q, want %q", field, got, want)
}

// TestMonitorFlatStatus_StartupSync covers a restart mid-update: status:mdb
// already holds "downloading" before the watcher subscribes, and nothing
// touches either status field afterwards. The flat pair must still reflect
// it, or a restart during an update would leave a stale (or blank) flat pair
// until the next state transition.
func TestMonitorFlatStatus_StartupSync(t *testing.T) {
	u, mdbStatus, _, mr := newTestUpdaterForFlatStatus(t)

	if err := mdbStatus.SetDownloading(context.Background(), "v1.2.3", "full"); err != nil {
		t.Fatal(err)
	}

	go u.monitorFlatStatus()

	waitForFlatField(t, mr, "status", "downloading-updates")
	waitForFlatField(t, mr, "update-type", "blocking")
}

// TestMonitorFlatStatus_EarliestStageWins drives both components through a
// full update to check the precedence rule end to end, not just FlatFor in
// isolation: the flat pair must track the least-advanced side and only clear
// once both are idle again.
func TestMonitorFlatStatus_EarliestStageWins(t *testing.T) {
	u, mdbStatus, dbcStatus, mr := newTestUpdaterForFlatStatus(t)
	ctx := context.Background()

	go u.monitorFlatStatus()
	waitForFlatField(t, mr, "status", "")

	if err := dbcStatus.SetDownloading(ctx, "v1.2.3", "full"); err != nil {
		t.Fatal(err)
	}
	waitForFlatField(t, mr, "status", "downloading-updates")

	// MDB reaching pending-reboot while DBC is still downloading must not
	// mask the still-busy DBC.
	if err := mdbStatus.SetDownloading(ctx, "v1.2.3", "full"); err != nil {
		t.Fatal(err)
	}
	if err := mdbStatus.SetInstalling(ctx); err != nil {
		t.Fatal(err)
	}
	if err := mdbStatus.SetPendingReboot(ctx); err != nil {
		t.Fatal(err)
	}
	waitForFlatField(t, mr, "status", "downloading-updates")

	if err := dbcStatus.SetInstalling(ctx); err != nil {
		t.Fatal(err)
	}
	waitForFlatField(t, mr, "status", "installing-updates")

	if err := dbcStatus.SetPendingReboot(ctx); err != nil {
		t.Fatal(err)
	}
	waitForFlatField(t, mr, "status", "installation-complete-waiting-reboot")

	if err := mdbStatus.SetIdle(ctx); err != nil {
		t.Fatal(err)
	}
	// MDB alone going idle must not clear the pair while DBC is still
	// pending-reboot.
	if got := mr.HGet("ota", "status"); got != "installation-complete-waiting-reboot" {
		t.Errorf("status = %q, want installation-complete-waiting-reboot to survive mdb alone going idle", got)
	}

	if err := dbcStatus.SetIdle(ctx); err != nil {
		t.Fatal(err)
	}
	waitForFlatField(t, mr, "status", "")
	waitForFlatField(t, mr, "update-type", "")
}

// TestMonitorFlatStatus_IgnoresUnrelatedFields checks that a field other
// than status:mdb/status:dbc (e.g. the frequent download-bytes tick) does
// not trigger a write, by asserting the flat pair is untouched afterwards.
func TestMonitorFlatStatus_IgnoresUnrelatedFields(t *testing.T) {
	u, mdbStatus, _, mr := newTestUpdaterForFlatStatus(t)
	ctx := context.Background()

	go u.monitorFlatStatus()
	waitForFlatField(t, mr, "status", "")

	if err := mdbStatus.SetDownloading(ctx, "v1.2.3", "full"); err != nil {
		t.Fatal(err)
	}
	waitForFlatField(t, mr, "status", "downloading-updates")

	if err := mdbStatus.SetDownloadProgress(ctx, 5000, 10000); err != nil {
		t.Fatal(err)
	}
	// download-progress/-bytes/-total churn every tick; give a settle window
	// and confirm the flat pair is exactly what SetDownloading already wrote,
	// not merely eventually consistent with it.
	time.Sleep(50 * time.Millisecond)
	if got := mr.HGet("ota", "status"); got != "downloading-updates" {
		t.Errorf("status = %q, want unaffected by a download-progress tick", got)
	}
}
