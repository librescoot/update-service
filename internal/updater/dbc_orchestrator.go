package updater

import (
	"context"
	"strings"
	"time"

	"github.com/librescoot/update-service/internal/config"
)

const (
	dbcActivityTimeout = 60 * time.Second
	dbcPollInterval    = 5 * time.Second
)

// orchestrateDBC is called from checkForUpdates() on MDB when orchestration is enabled.
// It checks if a DBC update is available and, if so, powers on the DBC, triggers
// a check, and monitors for activity.
func (u *Updater) orchestrateDBC(releases []Release) {
	if u.config.Component != "mdb" {
		return
	}

	if !u.getOrchestrateDBC() {
		return
	}

	// Only orchestrate in stand-by. In any other state the user may be driving
	// or otherwise interacting with the DBC, and toggling its power is unsafe.
	state, err := u.redis.GetVehicleState(config.VehicleHashKey)
	if err != nil {
		u.logger.Printf("[dbc-orchestrate] Failed to read vehicle state: %v, skipping", err)
		return
	}
	if state != "stand-by" {
		u.logger.Printf("[dbc-orchestrate] Vehicle state is '%s' (not stand-by), skipping", state)
		return
	}

	// Prevent overlapping orchestrations
	if !u.dbcOrchestrating.TryLock() {
		u.logger.Printf("[dbc-orchestrate] Already in progress, skipping")
		return
	}
	defer u.dbcOrchestrating.Unlock()

	if !u.isDBCUpdateAvailable(releases) {
		return
	}

	// Check if DBC is already updating
	dbcStatus, err := u.redis.GetDBCOTAStatus()
	if err == nil && dbcStatus != "" && dbcStatus != "idle" {
		u.logger.Printf("[dbc-orchestrate] DBC already in '%s' state, skipping", dbcStatus)
		return
	}

	u.logger.Printf("[dbc-orchestrate] DBC update available, powering on dashboard")

	// Power on DBC and queue the check-now command. The command goes into a
	// Redis LPUSH queue (scooter:update:dbc) which persists until consumed by
	// the DBC's update-service via BRPOP. No readiness check needed — even if
	// the DBC hasn't fully booted yet, the command will be picked up when its
	// update-service starts listening.
	if err := u.redis.SendHardwareCommand("dashboard:on"); err != nil {
		u.logger.Printf("[dbc-orchestrate] Failed to send dashboard:on: %v", err)
		return
	}

	if err := u.redis.PushUpdateCommandToComponent("dbc", "check-now"); err != nil {
		u.logger.Printf("[dbc-orchestrate] Failed to send check-now to DBC: %v", err)
		return
	}

	u.logger.Printf("[dbc-orchestrate] Dashboard powered on, check-now queued")

	// Watch for DBC update activity
	result := u.watchDBCActivity()
	switch result {
	case dbcWatchStarted:
		u.logger.Printf("[dbc-orchestrate] DBC update started, handing off to DBC update-service")
	case dbcWatchVehicleWoke:
		u.logger.Printf("[dbc-orchestrate] Vehicle left stand-by during watch, leaving dashboard on")
	case dbcWatchTimeout:
		// Re-check vehicle state before powering off — the user may have
		// woken the scooter in the race between the last poll and now.
		currentState, err := u.redis.GetVehicleState(config.VehicleHashKey)
		if err != nil {
			u.logger.Printf("[dbc-orchestrate] Timeout reached but failed to re-check vehicle state: %v, leaving dashboard on", err)
			return
		}
		if currentState != "stand-by" {
			u.logger.Printf("[dbc-orchestrate] Timeout reached but vehicle is in '%s', leaving dashboard on", currentState)
			return
		}
		u.logger.Printf("[dbc-orchestrate] DBC did not start updating within %v, powering off", dbcActivityTimeout)
		if err := u.redis.SendHardwareCommand("dashboard:off"); err != nil {
			u.logger.Printf("[dbc-orchestrate] Failed to send dashboard:off: %v", err)
		}
	}
}

// isDBCUpdateAvailable checks if a newer release exists for the DBC component.
func (u *Updater) isDBCUpdateAvailable(releases []Release) bool {
	// Get DBC variant ID
	dbcVariantID, err := u.redis.GetVariantID("dbc")
	if err != nil {
		u.logger.Printf("[dbc-orchestrate] Failed to get DBC variant_id: %v", err)
		return false
	}

	// Determine DBC channel
	dbcChannel := u.getDBCChannel()

	// Find the latest release for DBC
	release, found := u.findLatestRelease(releases, dbcVariantID, dbcChannel)
	if !found {
		return false
	}

	// Get DBC's current version
	dbcVersion, err := u.redis.GetComponentVersion("dbc")
	if err != nil || dbcVersion == "" {
		// DBC version unknown (off, never booted, or version not persisted to MDB Redis).
		// A release exists for DBC, so power it on and let its own update-service decide.
		u.logger.Printf("[dbc-orchestrate] DBC version unknown, release %s exists — will power on to check", release.TagName)
		return true
	}

	// Compare versions using existing logic
	return u.isVersionNewer(release.TagName, dbcVersion, dbcChannel)
}

// getDBCChannel determines the effective channel for DBC updates.
// Priority: Redis setting > inferred from DBC version > MDB's own channel.
func (u *Updater) getDBCChannel() string {
	// Try explicit Redis setting
	if ch := u.redis.GetDBCChannel(); ch != "" && config.IsValidChannel(ch) {
		return ch
	}

	// Try inferring from installed DBC version
	if dbcVersion, err := u.redis.GetComponentVersion("dbc"); err == nil && dbcVersion != "" {
		if ch := config.InferChannelFromVersion(dbcVersion); ch != "" {
			return ch
		}
	}

	// Fall back to MDB's own channel
	return u.config.Channel
}

// isVersionNewer checks if releaseTag is newer than installedVersion for the given channel.
func (u *Updater) isVersionNewer(releaseTag, installedVersion, channel string) bool {
	if channel == "stable" {
		normInstalled := installedVersion
		if !strings.HasPrefix(normInstalled, "v") {
			normInstalled = "v" + normInstalled
		}
		return compareVersions(releaseTag, normInstalled) > 0
	}

	// Nightly/testing: tags are like "nightly-20260407-abc123", lexicographic > means newer
	normRelease := strings.ToLower(releaseTag)
	normInstalled := strings.ToLower(installedVersion)

	// Handle legacy short versions
	if !strings.HasPrefix(normInstalled, channel+"-") {
		parts := strings.Split(normRelease, "-")
		if len(parts) >= 2 && normInstalled == parts[1] {
			return false
		}
	}

	return normRelease > normInstalled
}

type dbcWatchResult int

const (
	dbcWatchTimeout dbcWatchResult = iota
	dbcWatchStarted
	dbcWatchVehicleWoke
)

// watchDBCActivity polls the DBC OTA status and vehicle state. Returns
// dbcWatchStarted if the DBC transitions to an active update state,
// dbcWatchVehicleWoke if the vehicle leaves stand-by, or dbcWatchTimeout
// otherwise.
func (u *Updater) watchDBCActivity() dbcWatchResult {
	ctx, cancel := context.WithTimeout(u.ctx, dbcActivityTimeout)
	defer cancel()

	ticker := time.NewTicker(dbcPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return dbcWatchTimeout
		case <-ticker.C:
			if state, err := u.redis.GetVehicleState(config.VehicleHashKey); err == nil && state != "stand-by" {
				return dbcWatchVehicleWoke
			}
			status, err := u.redis.GetDBCOTAStatus()
			if err != nil {
				continue
			}
			switch status {
			case "downloading", "preparing", "installing", "pending-reboot":
				return dbcWatchStarted
			}
		}
	}
}
