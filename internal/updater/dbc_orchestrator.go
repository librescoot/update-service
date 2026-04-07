package updater

import (
	"context"
	"strings"
	"time"

	"github.com/librescoot/update-service/internal/config"
)

const (
	dbcReadyTimeout    = 30 * time.Second
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

	// Power on DBC
	if err := u.redis.SendHardwareCommand("dashboard:on"); err != nil {
		u.logger.Printf("[dbc-orchestrate] Failed to send dashboard:on: %v", err)
		return
	}

	// Wait for DBC to become ready (version:dbc hash gets populated on boot)
	if !u.waitForDBCReady() {
		u.logger.Printf("[dbc-orchestrate] DBC did not become ready within %v, powering off", dbcReadyTimeout)
		if err := u.redis.SendHardwareCommand("dashboard:off"); err != nil {
			u.logger.Printf("[dbc-orchestrate] Failed to send dashboard:off: %v", err)
		}
		return
	}

	u.logger.Printf("[dbc-orchestrate] DBC ready, sending check-now")

	// Trigger DBC update check
	if err := u.redis.PushUpdateCommandToComponent("dbc", "check-now"); err != nil {
		u.logger.Printf("[dbc-orchestrate] Failed to send check-now to DBC: %v", err)
		return
	}

	// Watch for DBC update activity
	if !u.watchDBCActivity() {
		u.logger.Printf("[dbc-orchestrate] DBC did not start updating within %v, powering off", dbcActivityTimeout)
		if err := u.redis.SendHardwareCommand("dashboard:off"); err != nil {
			u.logger.Printf("[dbc-orchestrate] Failed to send dashboard:off: %v", err)
		}
		return
	}

	u.logger.Printf("[dbc-orchestrate] DBC update started, handing off to DBC update-service")
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
		// Can't determine DBC version (maybe it's off and never published).
		// If we have no version info at all, we can't tell if an update is needed.
		u.logger.Printf("[dbc-orchestrate] Cannot determine DBC version (off or never booted?), skipping")
		return false
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

	// Nightly/testing: lexicographic comparison of lowercased tags
	normRelease := strings.ToLower(releaseTag)
	normInstalled := strings.ToLower(installedVersion)

	// Handle legacy short versions
	if !strings.HasPrefix(normInstalled, channel+"-") {
		parts := strings.Split(normRelease, "-")
		if len(parts) >= 2 && normInstalled == parts[1] {
			return false // same version
		}
	}

	return normRelease != normInstalled
}

// waitForDBCReady polls until the DBC's version hash is populated in Redis,
// indicating it has booted and its version-service has started.
func (u *Updater) waitForDBCReady() bool {
	ctx, cancel := context.WithTimeout(u.ctx, dbcReadyTimeout)
	defer cancel()

	ticker := time.NewTicker(dbcPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			version, err := u.redis.GetComponentVersion("dbc")
			if err == nil && version != "" {
				return true
			}
		}
	}
}

// watchDBCActivity polls the DBC OTA status. Returns true if DBC transitions
// to an active update state within the timeout, false otherwise.
func (u *Updater) watchDBCActivity() bool {
	ctx, cancel := context.WithTimeout(u.ctx, dbcActivityTimeout)
	defer cancel()

	ticker := time.NewTicker(dbcPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			status, err := u.redis.GetDBCOTAStatus()
			if err != nil {
				continue
			}
			switch status {
			case "downloading", "preparing", "installing", "pending-reboot":
				return true
			}
		}
	}
}
