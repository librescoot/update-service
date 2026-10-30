package updater

import (
	"fmt"
	"os"
	"time"

	"github.com/librescoot/update-service/internal/redis"
)

func mdbRebootWaitReason(state redis.MDBRebootState) string {
	if state.VehicleState != "stand-by" {
		return "vehicle is not in stand-by"
	}
	// Vehicle leaves this field absent until the first DBC update lifecycle.
	if state.DBCUpdating != "" && state.DBCUpdating != "false" {
		return "DBC update lifecycle has not finished"
	}
	for _, status := range []string{state.DBCStatus, state.DBCBootStatus} {
		switch status {
		case "", "idle", "error", "staged-noop":
		default:
			return "DBC update has not reached a final outcome"
		}
	}
	if state.DashboardPower != "off" {
		return "dashboard shutdown has not finished"
	}
	return ""
}

func (u *Updater) triggerMDBRebootIfUnowned(requiredStandby time.Duration) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	lastReason := ""
	for {
		if err := u.ctx.Err(); err != nil {
			return err
		}
		if _, err := os.Stat(umsMDBRebootOwnerPath); err == nil {
			u.logger.Printf("MDB reboot is owned by UMS (local claim); leaving update pending")
			return nil
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("read local MDB reboot owner: %w", err)
		}

		state, err := u.redis.GetMDBRebootState(u.ctx)
		reason := "cannot read MDB reboot prerequisites"
		if err == nil {
			if state.Owner == "ums" {
				u.logger.Printf("MDB reboot is owned by UMS; leaving update pending")
				return nil
			}
			if state.VehicleState != "stand-by" {
				u.setStandbyStartTime(time.Time{})
			} else if !state.StateSince.IsZero() {
				u.setStandbyStartTime(state.StateSince)
			} else if u.getStandbyStartTime().IsZero() {
				u.setStandbyStartTime(time.Now())
			}
			reason = mdbRebootWaitReason(state)
			if reason == "" && time.Since(u.getStandbyStartTime()) < requiredStandby {
				reason = "required stand-by duration has not elapsed"
			}
			if reason == "" {
				u.logger.Printf("DBC update finished and dashboard powered off; requesting MDB reboot")
				return u.redis.TriggerReboot()
			}
		}
		if reason != lastReason {
			u.logger.Printf("Deferring MDB reboot: %s", reason)
			lastReason = reason
		}
		select {
		case <-u.ctx.Done():
			return u.ctx.Err()
		case <-ticker.C:
		}
	}
}
