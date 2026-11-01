package updater

import (
	"context"
	"io"
	"log"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/librescoot/update-service/internal/config"
	"github.com/librescoot/update-service/internal/redis"
	"github.com/librescoot/update-service/internal/status"
)

func bootRecoveryFixture(t *testing.T, component string) (*Updater, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rc, err := redis.New(mr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rc.Close() })
	logger := log.New(io.Discard, "", 0)
	return &Updater{
		config:     &config.Config{Component: component},
		redis:      rc,
		status:     status.NewReporter(rc.GetClient(), component, logger),
		bootStatus: status.NewReporter(rc.GetClient(), component+"-boot", logger),
		logger:     logger,
		ctx:        context.Background(),
		bootID:     func() (string, error) { return "current-boot", nil },
	}, mr
}

func TestBootRecoveryWithSettledRootfs(t *testing.T) {
	for _, component := range []string{"mdb", "dbc"} {
		for _, rootStatus := range []string{"", "idle"} {
			for _, bootStatus := range []string{"", "idle", "pending-reboot", "downloading", "preparing", "installing", "error", "unknown"} {
				t.Run(component+"/root="+rootStatus+"/boot="+bootStatus, func(t *testing.T) {
					u, mr := bootRecoveryFixture(t, component)
					rootKey, bootKey := "status:"+component, "status:"+component+"-boot"
					if rootStatus != "" {
						mr.HSet("ota", rootKey, rootStatus)
					}
					if bootStatus != "" {
						mr.HSet("ota", bootKey, bootStatus)
					}
					if component == "dbc" && bootStatus == "pending-reboot" {
						mr.HSet("ota", dbcBootRebootID, "previous-boot")
					}
					if err := u.recoverFromStuckState(false, false); err != nil {
						t.Fatal(err)
					}
					want := bootStatus
					switch bootStatus {
					case "pending-reboot", "downloading", "preparing", "error":
						want = "idle"
					case "installing":
						if component != "dbc" {
							want = "idle"
						}
					}
					if got := mr.HGet("ota", bootKey); got != want {
						t.Fatalf("boot status = %q, want %q", got, want)
					}
					if got := mr.HGet("ota", rootKey); got != rootStatus {
						t.Fatalf("settled rootfs status changed to %q", got)
					}
					if component == "dbc" && bootStatus == "pending-reboot" {
						commands, err := mr.List("scooter:update")
						if err != nil || len(commands) != 1 || commands[0] != "complete-dbc" {
							t.Fatalf("boot completion commands = %v, error = %v", commands, err)
						}
					} else if mr.Exists("scooter:update") {
						t.Fatal("unexpected DBC lifecycle command")
					}
				})
			}
		}
	}
}

func TestBootRecoveryWithoutBootReporter(t *testing.T) {
	u, mr := bootRecoveryFixture(t, "dbc")
	u.bootStatus = nil
	mr.HSet("ota", "status:dbc", "idle")
	if err := u.recoverFromStuckState(false, false); err != nil {
		t.Fatal(err)
	}
	if mr.Exists("scooter:update") {
		t.Fatal("unexpected lifecycle command without boot reporter")
	}
}

func TestBootRecoveryPreservesPendingRootfs(t *testing.T) {
	for _, gateOpen := range []bool{false, true} {
		u, mr := bootRecoveryFixture(t, "dbc")
		mr.HSet("ota", "status:dbc", "pending-reboot", "status:dbc-boot", "pending-reboot")
		mr.HSet("vehicle", "state", "stand-by", "dashboard:power", "off", "dbc-updating", "false")
		if err := u.recoverFromStuckState(!gateOpen, gateOpen); err != nil {
			t.Fatal(err)
		}
		if got := mr.HGet("ota", "status:dbc"); got != "pending-reboot" {
			t.Fatalf("rootfs awaiting reboot/commit was cleared: %q", got)
		}
		if got := mr.HGet("ota", "status:dbc-boot"); got != "pending-reboot" || mr.Exists("scooter:update") {
			t.Fatalf("rootfs gate released boot lifecycle: boot=%q", got)
		}
		state, err := u.redis.GetMDBRebootState(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if reason := mdbRebootWaitReason(state); reason == "" {
			t.Fatal("MDB reboot allowed before rootfs reboot/commit completed")
		}
	}
}

func TestBootRecoveryPreservesMDBRebootGuards(t *testing.T) {
	u, mr := bootRecoveryFixture(t, "dbc")
	mr.HSet("ota", "status:dbc", "idle", "status:dbc-boot", "pending-reboot", dbcBootRebootID, "previous-boot")
	mr.HSet("vehicle", "state", "stand-by", "dashboard:power", "on", "dbc-updating", "true")
	if err := u.recoverFromStuckState(false, false); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		field, value string
		blocked      bool
	}{
		{"dbc-updating", "true", true},
		{"dbc-updating", "false", true},
		{"dashboard:power", "off", false},
		{"state", "ready-to-drive", true},
	} {
		mr.HSet("vehicle", step.field, step.value)
		state, err := u.redis.GetMDBRebootState(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if reason := mdbRebootWaitReason(state); (reason != "") != step.blocked {
			t.Fatalf("%s=%s: reboot wait reason = %q, want blocked=%v", step.field, step.value, reason, step.blocked)
		}
	}
}
