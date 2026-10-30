package updater

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/librescoot/update-service/internal/redis"
)

func TestMDBRebootRequiresFinishedDBCAndPowerOff(t *testing.T) {
	ready := redis.MDBRebootState{VehicleState: "stand-by", DashboardPower: "off", DBCUpdating: "false", DBCStatus: "idle", DBCBootStatus: "idle"}
	if reason := mdbRebootWaitReason(ready); reason != "" {
		t.Fatal(reason)
	}
	for _, status := range []string{"downloading", "preparing", "installing", "pending-reboot", "unknown"} {
		for _, component := range []string{"rootfs", "boot"} {
			t.Run(component+"/"+status, func(t *testing.T) {
				state := ready
				if component == "rootfs" {
					state.DBCStatus = status
				} else {
					state.DBCBootStatus = status
				}
				if mdbRebootWaitReason(state) == "" {
					t.Fatal("unfinished DBC update admitted MDB reboot")
				}
				state.DBCUpdating = ""
				if mdbRebootWaitReason(state) == "" {
					t.Fatal("missing lifecycle flag bypassed unfinished DBC update")
				}
			})
		}
	}
	for _, field := range []string{"vehicle", "power", "lifecycle"} {
		for _, value := range []string{"", "unknown", "on", "true", "parked", "shutting-down"} {
			if field == "lifecycle" && value == "" {
				continue
			}
			state := ready
			switch field {
			case "vehicle":
				state.VehicleState = value
			case "power":
				state.DashboardPower = value
			case "lifecycle":
				state.DBCUpdating = value
			}
			if mdbRebootWaitReason(state) == "" {
				t.Fatalf("accepted %s=%q", field, value)
			}
		}
	}
	for _, status := range []string{"", "idle", "error", "staged-noop"} {
		state := ready
		state.DBCStatus, state.DBCBootStatus = status, status
		if reason := mdbRebootWaitReason(state); reason != "" {
			t.Fatalf("terminal %q rejected: %s", status, reason)
		}
	}
}

func TestMDBRebootWaitsForDBCCompletionBeforeRequestingPower(t *testing.T) {
	u, mr := newPendingCommitUpdater(t)
	mr.HSet("vehicle", "state", "stand-by", "dashboard:power", "on", "dbc-updating", "true")
	mr.HSet("ota", "status:dbc", "pending-reboot")
	u.setStandbyStartTime(time.Now().Add(-time.Minute))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	u.ctx = ctx
	done := make(chan error, 1)
	go func() { done <- u.TriggerReboot("mdb", true) }()
	defer func() { cancel(); <-done }()

	// Simulate the DBC reporting idle before its lifecycle and power-down complete.
	for _, step := range []func(){
		func() {},
		func() { mr.HSet("ota", "status:dbc", "idle") },
		func() { mr.HSet("vehicle", "dbc-updating", "false") },
	} {
		step()
		time.Sleep(100 * time.Millisecond)
		if mr.Exists("scooter:power") {
			t.Fatal("MDB reboot requested before DBC completion and power-off")
		}
	}
	mr.HSet("vehicle", "dashboard:power", "off")
	deadline := time.Now().Add(3 * time.Second)
	for !mr.Exists("scooter:power") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	commands, err := mr.List("scooter:power")
	if err != nil || len(commands) != 1 || commands[0] != "reboot" {
		t.Fatalf("power requests = %v, %v", commands, err)
	}
}

func TestMDBRebootFinalGateFailsClosed(t *testing.T) {
	for _, scenario := range []string{"missing", "redis-error", "pending-rootfs", "pending-boot", "dashboard-on", "vehicle-left-standby", "automatic-duration"} {
		t.Run(scenario, func(t *testing.T) {
			u, mr := newPendingCommitUpdater(t)
			mr.HSet("vehicle", "state", "stand-by", "dashboard:power", "off", "dbc-updating", "false", "state:timestamp", time.Now().Format(time.RFC3339Nano))
			mr.HSet("ota", "status:dbc", "idle")
			required := time.Duration(0)
			switch scenario {
			case "missing":
				mr.Del("vehicle")
			case "redis-error":
				mr.Del("vehicle")
				mr.Set("vehicle", "wrong-type")
			case "pending-rootfs":
				mr.HSet("ota", "status:dbc", "pending-reboot")
			case "pending-boot":
				mr.HSet("ota", "status:dbc-boot", "pending-reboot")
			case "dashboard-on":
				mr.HSet("vehicle", "dashboard:power", "on")
			case "vehicle-left-standby":
				mr.HSet("vehicle", "state", "ready-to-drive")
			case "automatic-duration":
				u.setStandbyStartTime(time.Now().Add(-time.Hour))
				required = 3 * time.Minute
			}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			u.ctx = ctx
			if err := u.triggerMDBRebootIfUnowned(required); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("gate returned %v", err)
			}
			if mr.Exists("scooter:power") {
				t.Fatal("unsafe reboot request queued")
			}
		})
	}
}

func TestMDBRebootWithoutPreviousDBCUpdate(t *testing.T) {
	u, mr := newPendingCommitUpdater(t)
	// These are the vehicle's cold-start fields; no DBC update has run.
	mr.HSet("vehicle", "state", "stand-by", "dashboard:power", "off")
	u.setStandbyStartTime(time.Now().Add(-time.Minute))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	u.ctx = ctx
	if err := u.TriggerReboot("mdb", true); err != nil {
		t.Fatalf("MDB-only update blocked with no DBC lifecycle: %v", err)
	}
	commands, err := mr.List("scooter:power")
	if err != nil || len(commands) != 1 || commands[0] != "reboot" {
		t.Fatalf("power requests = %v, %v", commands, err)
	}
}

func TestMDBRebootHonorsUMSOwnership(t *testing.T) {
	u, mr := newPendingCommitUpdater(t)
	mr.HSet("ota", "reboot-owner:mdb", "ums")
	if err := u.triggerMDBRebootIfUnowned(0); err != nil {
		t.Fatal(err)
	}
	if mr.Exists("scooter:power") {
		t.Fatal("reboot queued despite UMS ownership")
	}
}
