package updater

import (
	"github.com/librescoot/update-service/internal/config"
)

// monitorFlatStatus keeps the flat `status` / `update-type` pair on the ota
// hash mirroring whichever of MDB or DBC is least advanced through an
// update (see status.FlatFor). MDB-only: Start only spawns this when
// flatMirror is set, which happens under the same "mdb" guard as dbcStatus.
func (u *Updater) monitorFlatStatus() {
	recompute := func(string) error {
		mdbStatus, err := u.status.GetStatus(u.ctx)
		if err != nil {
			u.logger.Printf("[flat-status] Failed to read mdb status: %v", err)
			return nil
		}
		dbcStatus, err := u.dbcStatus.GetStatus(u.ctx)
		if err != nil {
			u.logger.Printf("[flat-status] Failed to read dbc status: %v", err)
			return nil
		}
		if err := u.flatMirror.Write(u.ctx, mdbStatus, dbcStatus); err != nil {
			u.logger.Printf("[flat-status] Failed to write flat status: %v", err)
		}
		return nil
	}

	watcher := u.redis.NewOTAWatcher(config.OtaStatusHashKey)
	watcher.OnField("status:mdb", recompute)
	watcher.OnField("status:dbc", recompute)

	// StartWithSync replays the hash's current status:mdb/status:dbc through
	// recompute right after subscribing. Without it a restart that lands
	// between two status changes (e.g. mid pending-reboot) would never
	// recompute the pair until the next transition, leaving a stale value
	// from whatever the previous process last wrote.
	if err := watcher.StartWithSync(); err != nil {
		u.logger.Printf("[flat-status] Failed to start watcher: %v", err)
		return
	}

	<-u.ctx.Done()
	_ = watcher.Stop()
}
