package updater

import (
	"context"
	"errors"
	"testing"
)

func TestUncertainDBCFlashBlocksMDBAfterWatchdogAndRestart(t *testing.T) {
	for _, failure := range []string{"writer", "identity"} {
		t.Run(failure, func(t *testing.T) {
			u, mr := bootRecoveryFixture(t, "dbc")
			mr.HSet("ota", "status:dbc", "idle")
			mr.HSet("vehicle", "state", "stand-by", "dashboard:power", "on", "dbc-updating", "false")
			f := newFakeBoot()
			d := f.deps()
			d.status = u.bootStatus
			d.heartbeat = u.status
			d.dbcUpdating = func(context.Context) (bool, error) { return mr.HGet("vehicle", "dbc-updating") == "true", nil }
			d.command = func(ctx context.Context, cmd string) error {
				if err := u.redis.GetClient().Raw().LPush(ctx, "scooter:update", cmd).Err(); err != nil {
					return err
				}
				if cmd == "start-dbc" {
					mr.HSet("vehicle", "dbc-updating", "true")
				}
				return nil
			}
			if failure == "writer" {
				f.applyErr = errBootTest
			} else {
				d.prepareReboot = func(context.Context) error { return errBootTest }
			}
			applied, err := runLocalBootUpdate(u.ctx, "dbc", d)
			if applied || !errors.Is(err, errDBCBootOutcomeUnknown) {
				t.Fatalf("applied=%v err=%v", applied, err)
			}
			commands, err := mr.List("scooter:update")
			if err != nil || len(commands) != 1 || commands[0] != "start-dbc" {
				t.Fatalf("commands=%v err=%v", commands, err)
			}
			if mr.HGet("ota", "status:dbc-boot") != "installing" {
				t.Fatal("uncertain flash became terminal")
			}
			// Model the vehicle watchdog releasing the lifecycle and powering off.
			mr.HSet("vehicle", "dbc-updating", "false", "dashboard:power", "off")
			if err := u.recoverFromStuckState(false, false); err != nil {
				t.Fatal(err)
			}
			state, err := u.redis.GetMDBRebootState(u.ctx)
			if err != nil || mdbRebootWaitReason(state) == "" {
				t.Fatalf("MDB not blocked by uncertain flash: %+v, %v", state, err)
			}
		})
	}
}
