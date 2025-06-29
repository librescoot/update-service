package updater

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/librescoot/update-service/internal/config"
	"github.com/librescoot/update-service/internal/inhibitor"
	"github.com/librescoot/update-service/internal/mender"
	"github.com/librescoot/update-service/internal/power"
	"github.com/librescoot/update-service/internal/redis"
	"github.com/librescoot/update-service/internal/status"
)

// Updater represents the component-aware update orchestrator
type Updater struct {
	config    *config.Config
	redis     *redis.Client // Client from internal/redis
	inhibitor *inhibitor.Client
	power     *power.Client
	mender    *mender.Manager
	status    *status.Reporter
	githubAPI *GitHubAPI
	logger    *log.Logger
	ctx       context.Context
	cancel    context.CancelFunc
}

// New creates a new component-aware updater
func New(ctx context.Context, cfg *config.Config, redisClient *redis.Client, inhibitorClient *inhibitor.Client, powerClient *power.Client, logger *log.Logger) *Updater {
	updaterCtx, cancel := context.WithCancel(ctx)

	// Create download directory in /data/ota/{component}
	downloadDir := filepath.Join("/data/ota", cfg.Component)

	return &Updater{
		config:    cfg,
		redis:     redisClient,
		inhibitor: inhibitorClient,
		power:     powerClient,
		mender:    mender.NewManager(downloadDir, logger),
		status:    status.NewReporter(redisClient.GetClient(), cfg.Component, logger), // status.NewReporter expects the underlying go-redis/v9 client
		githubAPI: NewGitHubAPI(updaterCtx, cfg.GitHubReleasesURL, logger),
		logger:    logger,
		ctx:       updaterCtx,
		cancel:    cancel,
	}
}

// CheckAndCommitPendingUpdate checks for and commits any pending updates on startup
func (u *Updater) CheckAndCommitPendingUpdate() error {
	u.logger.Printf("Checking for pending updates to commit for component %s", u.config.Component)

	needsCommit, err := u.mender.NeedsCommit()
	if err != nil {
		return fmt.Errorf("failed to check if commit is needed: %w", err)
	}

	if needsCommit {
		u.logger.Printf("Found pending update for %s, attempting to commit...", u.config.Component)
		// Attempt to commit, log outcome (mender.Commit logs details), but don't let failure here stop startup.
		if errCommit := u.mender.Commit(); errCommit != nil {
			u.logger.Printf("Attempt to commit pending Mender update for %s during startup resulted in an error (continuing startup): %v", u.config.Component, errCommit)
		} else {
			u.logger.Printf("Attempt to commit pending Mender update for %s during startup completed.", u.config.Component)
		}
		// Regardless of commit outcome, we consider this check handled for startup purposes.
	} else {
		u.logger.Printf("No pending update to commit for %s", u.config.Component)
	}

	return nil
}

// Start starts the updater
func (u *Updater) Start() error {
	u.logger.Printf("Starting component-aware updater for %s", u.config.Component)

	// Clear rebooting status if present (reboot completed or failed)
	if err := u.clearRebootingStatus(); err != nil {
		u.logger.Printf("Warning: Failed to clear rebooting status: %v", err)
	}

	// Start the update check loop
	go u.updateCheckLoop()

	return nil
}

// Stop stops the updater
func (u *Updater) Stop() {
	u.cancel()
}

// Close performs cleanup when the updater is shutting down
func (u *Updater) Close() {
	u.logger.Printf("Shutting down updater for component %s", u.config.Component)
	u.Stop()
}

// clearRebootingStatus clears the rebooting status if present on startup
func (u *Updater) clearRebootingStatus() error {
	currentStatus, err := u.status.GetStatus(u.ctx)
	if err != nil {
		return fmt.Errorf("failed to get current status: %w", err)
	}

	if currentStatus == status.StatusRebooting {
		u.logger.Printf("Found rebooting status for %s on startup, clearing (reboot completed or failed)", u.config.Component)
		if err := u.status.SetIdleAndClearVersion(u.ctx); err != nil {
			return fmt.Errorf("failed to clear rebooting status: %w", err)
		}
		u.logger.Printf("Cleared rebooting status for %s", u.config.Component)
	}

	return nil
}

// updateCheckLoop periodically checks for updates
func (u *Updater) updateCheckLoop() {
	ticker := time.NewTicker(u.config.CheckInterval)
	defer ticker.Stop()

	// Do an initial check immediately
	u.checkForUpdates()

	for {
		select {
		case <-u.ctx.Done():
			u.logger.Printf("Update check loop stopped")
			return
		case <-ticker.C:
			u.checkForUpdates()
		}
	}
}

// checkForUpdates checks for updates and initiates the update process if updates are available
func (u *Updater) checkForUpdates() {
	u.logger.Printf("Checking for updates for component %s on channel %s", u.config.Component, u.config.Channel)

	// Check if we're waiting for a reboot - if so, defer updates
	currentStatus, err := u.status.GetStatus(u.ctx)
	if err != nil {
		u.logger.Printf("Failed to get current status: %v", err)
		return
	}

	if currentStatus == status.StatusRebooting {
		u.logger.Printf("Component %s is in rebooting state, deferring update check until reboot completes", u.config.Component)
		return
	}

	// Get releases from GitHub
	releases, err := u.githubAPI.GetReleases()
	if err != nil {
		u.logger.Printf("Failed to get releases: %v", err)
		return
	}

	// Find the latest release for our component and channel
	release, found := u.findLatestRelease(releases, u.config.Component, u.config.Channel)
	if !found {
		u.logger.Printf("No release found for component %s and channel %s", u.config.Component, u.config.Channel)
		return
	}

	// Find the .mender asset for the component
	var assetURL string
	for _, asset := range release.Assets {
		if strings.Contains(asset.Name, u.config.Component) && strings.HasSuffix(asset.Name, ".mender") {
			assetURL = asset.BrowserDownloadURL
			break
		}
	}

	if assetURL == "" {
		u.logger.Printf("No .mender asset found for component %s in release %s", u.config.Component, release.TagName)
		return
	}

	// Check if update is needed
	if !u.isUpdateNeeded(release) {
		u.logger.Printf("No update needed for component %s", u.config.Component)
		return
	}

	u.logger.Printf("Update needed for %s: %s", u.config.Component, release.TagName)

	// Start the update process
	go u.performUpdate(release, assetURL)
}

// findLatestRelease finds the latest release for the given component and channel
func (u *Updater) findLatestRelease(releases []Release, component, channel string) (Release, bool) {
	var latestRelease Release
	found := false

	for _, release := range releases {
		// Check if the release is for the specified channel
		if !strings.HasPrefix(release.TagName, channel+"-") {
			continue
		}

		// Check if the release has assets for the specified component
		hasComponentAsset := false
		for _, asset := range release.Assets {
			if strings.Contains(asset.Name, component) && strings.HasSuffix(asset.Name, ".mender") {
				hasComponentAsset = true
				break
			}
		}

		if !hasComponentAsset {
			continue
		}

		// If this is the first matching release or it's newer than the current latest
		if !found || release.PublishedAt.After(latestRelease.PublishedAt) {
			latestRelease = release
			found = true
		}
	}

	return latestRelease, found
}

// isUpdateNeeded checks if an update is needed for the current component
func (u *Updater) isUpdateNeeded(release Release) bool {
	// Get the currently installed version
	currentVersion, err := u.getCurrentVersion()
	if err != nil {
		u.logger.Printf("Failed to get current %s version: %v", u.config.Component, err)
		// If we can't get the current version, assume an update is needed
		return true
	}

	// If no version is installed, an update is needed
	if currentVersion == "" {
		u.logger.Printf("No %s version found, update needed", u.config.Component)
		return true
	}

	// Extract the timestamp part from the release tag (format: nightly-20250506T214046)
	parts := strings.Split(release.TagName, "-")
	if len(parts) < 2 {
		u.logger.Printf("Invalid release tag format: %s", release.TagName)
		return true
	}

	// Convert to lowercase for comparison
	normalizedReleaseVersion := strings.ToLower(parts[1])

	if currentVersion != normalizedReleaseVersion {
		u.logger.Printf("Update needed for %s: current=%s, release=%s", u.config.Component, currentVersion, normalizedReleaseVersion)
		return true
	}

	u.logger.Printf("No update needed for %s: current=%s, release=%s", u.config.Component, currentVersion, normalizedReleaseVersion)
	return false
}

// getCurrentVersion gets the current version for this component
func (u *Updater) getCurrentVersion() (string, error) {
	result, err := u.redis.GetClient().HGet(u.ctx, fmt.Sprintf("version:%s", u.config.Component), "version_id").Result()
	if err == nil && result != "" {
		return result, nil
	}

	// If not found in Redis, return empty (needs update)
	return "", nil
}

// performUpdate performs the actual update process
func (u *Updater) performUpdate(release Release, assetURL string) {
	u.logger.Printf("Starting update process for %s to version %s", u.config.Component, release.TagName)

	// Extract version from release tag
	parts := strings.Split(release.TagName, "-")
	if len(parts) < 2 {
		u.logger.Printf("Invalid release tag format: %s", release.TagName)
		if err := u.status.SetStatus(u.ctx, status.StatusError); err != nil {
			u.logger.Printf("Failed to set error status: %v", err)
		}
		return
	}
	version := strings.ToLower(parts[1])

	// Step 0: Check and commit any pending Mender update
	u.logger.Printf("Checking and attempting to commit any pending Mender update for %s before starting new update to %s", u.config.Component, release.TagName)
	// Attempt to commit. Log outcome (mender.Commit logs details), but don't let failure here stop the new update.
	if errCommit := u.mender.Commit(); errCommit != nil {
		u.logger.Printf("Attempt to commit pending Mender update for %s before new update to %s resulted in an error (proceeding with new update): %v", u.config.Component, release.TagName, errCommit)
	} else {
		u.logger.Printf("Attempt to commit pending Mender update for %s before new update to %s completed.", u.config.Component, release.TagName)
	}
	u.logger.Printf("Proceeding with update to %s for component %s.", release.TagName, u.config.Component)

	// Step 1: Set downloading status and add download inhibitor
	if err := u.status.SetStatusAndVersion(u.ctx, status.StatusDownloading, version); err != nil {
		u.logger.Printf("Failed to set downloading status: %v", err)
		return
	}

	// For DBC updates, notify vehicle-service to keep dashboard power on
	if u.config.Component == "dbc" {
		u.logger.Printf("Starting DBC update - sending start-dbc command")
		if err := u.redis.PushUpdateCommand("start-dbc"); err != nil {
			u.logger.Printf("Failed to send start-dbc command: %v", err)
		}
	}

	if err := u.inhibitor.AddDownloadInhibit(u.config.Component); err != nil {
		u.logger.Printf("Failed to add download inhibit: %v", err)
	}

	// Request ondemand CPU governor for optimal download performance
	if err := u.power.RequestOndemandGovernor(); err != nil {
		u.logger.Printf("Failed to request ondemand governor: %v", err)
	}

	defer func() {
		// Always clean up inhibitors on exit
		if err := u.inhibitor.RemoveDownloadInhibit(u.config.Component); err != nil {
			u.logger.Printf("Failed to remove download inhibit: %v", err)
		}
		if err := u.inhibitor.RemoveInstallInhibit(u.config.Component); err != nil {
			u.logger.Printf("Failed to remove install inhibit: %v", err)
		}

		// For DBC updates, notify vehicle-service that update is complete
		if u.config.Component == "dbc" {
			u.logger.Printf("DBC update cleanup - sending complete-dbc command")
			if err := u.redis.PushUpdateCommand("complete-dbc"); err != nil {
				u.logger.Printf("Failed to send complete-dbc command: %v", err)
			}
		}
	}()

	// Step 2: Download and verify the update
	filePath, err := u.mender.DownloadAndVerify(u.ctx, assetURL, "")
	if err != nil {
		u.logger.Printf("Failed to download update: %v", err)
		if err := u.status.SetStatus(u.ctx, status.StatusError); err != nil {
			u.logger.Printf("Failed to set error status: %v", err)
		}
		return
	}

	u.logger.Printf("Successfully downloaded update to: %s", filePath)

	// Step 3: Set installing status and add install inhibitor
	if err := u.status.SetStatus(u.ctx, status.StatusInstalling); err != nil {
		u.logger.Printf("Failed to set installing status: %v", err)
		return
	}

	// Add install inhibit before removing download inhibit to prevent a window where no inhibit is active
	if err := u.inhibitor.AddInstallInhibit(u.config.Component); err != nil {
		u.logger.Printf("Failed to add install inhibit: %v", err)
	}

	if err := u.inhibitor.RemoveDownloadInhibit(u.config.Component); err != nil {
		u.logger.Printf("Failed to remove download inhibit: %v", err)
	}

	// Step 4: Install the update
	if err := u.mender.Install(filePath); err != nil {
		u.logger.Printf("Failed to install update: %v", err)
		if err := u.status.SetStatus(u.ctx, status.StatusError); err != nil {
			u.logger.Printf("Failed to set error status: %v", err)
		}
		return
	}

	u.logger.Printf("Successfully installed update")

	// Step 5: Set rebooting status and prepare for reboot
	if err := u.status.SetStatus(u.ctx, status.StatusRebooting); err != nil {
		u.logger.Printf("Failed to set rebooting status: %v", err)
	}

	// Remove install inhibitor before reboot
	if err := u.inhibitor.RemoveInstallInhibit(u.config.Component); err != nil {
		u.logger.Printf("Failed to remove install inhibit: %v", err)
	}

	// Step 6: Trigger reboot (component will reboot automatically or system will reboot)
	u.logger.Printf("Update installation complete, system will reboot to apply changes")

	// Trigger reboot
	if u.config.Component == "mdb" || u.config.Component == "dbc" {
		u.logger.Printf("%s update installed, triggering reboot", strings.ToUpper(u.config.Component))
		err := u.TriggerReboot(u.config.Component) // Call the method on *Updater
		if err != nil {
			u.logger.Printf("Failed to trigger %s reboot: %v", u.config.Component, err)
			// If reboot trigger fails (and not due to dry run), set status to error
			// The TriggerReboot method now logs "DRY-RUN..." itself.
			// We check if the error message contains "DRY-RUN" to avoid setting error status.
			if !strings.Contains(err.Error(), "DRY-RUN") {
				if statusErr := u.status.SetStatus(u.ctx, status.StatusError); statusErr != nil {
					u.logger.Printf("Additionally failed to set error status after %s reboot trigger failure: %v", u.config.Component, statusErr)
				}
			}

			// If it was a dry run (error contains "DRY-RUN" or DryRun flag is true), simulate post-reboot.
			if u.config.DryRun || strings.Contains(err.Error(), "DRY-RUN") {
				u.logger.Printf("Dry run or simulated reboot: Simulating post-reboot state by setting idle status for %s.", u.config.Component)
				if idleErr := u.status.SetIdleAndClearVersion(u.ctx); idleErr != nil {
					u.logger.Printf("Failed to set idle status in dry run for %s: %v", u.config.Component, idleErr)
				}
			}
		}
		// If TriggerReboot was successful (and not a dry run), the system/component will reboot/restart.
		// Status remains 'rebooting'.
	} else {
		u.logger.Printf("Unknown component %s, cannot determine reboot strategy. Setting to idle.", u.config.Component)
		if err := u.status.SetIdleAndClearVersion(u.ctx); err != nil {
			u.logger.Printf("Failed to set idle status for unknown component: %v", err)
		}
	}
}

// IsSafeForDbcReboot checks if it's safe to reboot the DBC.
// DBC should not be rebooted when the scooter is being actively used.
func (u *Updater) IsSafeForDbcReboot() (bool, error) {
	currentState, err := u.redis.GetVehicleState(config.VehicleHashKey)
	if err != nil {
		return false, fmt.Errorf("failed to get current vehicle state: %w", err)
	}
	return currentState != "ready-to-drive" && currentState != "parked", nil
}

// TriggerReboot triggers a reboot or restart of the specified component.
func (u *Updater) TriggerReboot(component string) error {
	if u.config.DryRun {
		u.logger.Printf("DRY-RUN: Would reboot/restart %s, but dry-run mode is enabled", component)
		return fmt.Errorf("DRY-RUN: Would reboot/restart %s", component) // Return an error to signal dry run
	}

	switch component {
	case "mdb":
		u.logger.Printf("Preparing to reboot MDB. Waiting for vehicle to be in 'stand-by' state for at least 3 minutes.")
		const requiredStandbyDuration = 3 * time.Minute

		// Get current vehicle state and timestamp
		currentState, stateTimestamp, err := u.redis.GetVehicleStateWithTimestamp(config.VehicleHashKey)
		if err != nil {
			u.logger.Printf("Failed to get vehicle state for MDB reboot check: %v. Falling back to polling.", err)
			// Fall back to old polling behavior
			return u.waitForStandbyWithPolling(requiredStandbyDuration)
		}

		// Check if already in standby and for how long
		if currentState == "stand-by" && !stateTimestamp.IsZero() {
			durationInStandby := time.Since(stateTimestamp)
			if durationInStandby >= requiredStandbyDuration {
				u.logger.Printf("Vehicle already in 'stand-by' for %v (since %s). Proceeding with MDB reboot immediately.", durationInStandby, stateTimestamp.Format(time.RFC3339))
				u.logger.Printf("Triggering MDB reboot via Redis command")
				return u.redis.TriggerReboot()
			}
			remainingTime := requiredStandbyDuration - durationInStandby
			u.logger.Printf("Vehicle already in 'stand-by' for %v (since %s). Waiting %v more.", durationInStandby, stateTimestamp.Format(time.RFC3339), remainingTime)
			// Wait for the remaining time with more precise timing
			return u.waitForStandbyRemaining(stateTimestamp, requiredStandbyDuration)
		}

		// Vehicle not in standby or no timestamp, wait for standby state
		if currentState != "stand-by" {
			u.logger.Printf("Vehicle not in 'stand-by' (current: %s). Waiting for state change.", currentState)
		} else {
			u.logger.Printf("Vehicle in 'stand-by' but no timestamp available. Monitoring for state changes.")
		}

		// Subscribe to state changes for real-time updates
		return u.waitForStandbyWithSubscription(requiredStandbyDuration)

	case "dbc":
		safe, err := u.IsSafeForDbcReboot()
		if err != nil {
			return fmt.Errorf("failed to check DBC reboot safety: %w", err)
		}
		if !safe {
			currentState, _ := u.redis.GetVehicleState(config.VehicleHashKey)
			return fmt.Errorf("not safe to reboot DBC in current state: %s", currentState)
		}
		u.logger.Printf("Triggering DBC reboot via systemctl")
		if err := exec.Command("systemctl", "reboot").Run(); err != nil {
			return fmt.Errorf("failed to trigger reboot: %w", err)
		}
		return nil

	default:
		return fmt.Errorf("unknown component for reboot: %s", component)
	}
}

// waitForStandbyRemaining waits for the remaining time after vehicle is already in standby
func (u *Updater) waitForStandbyRemaining(standbyStartTime time.Time, requiredDuration time.Duration) error {
	remainingTime := requiredDuration - time.Since(standbyStartTime)
	if remainingTime <= 0 {
		u.logger.Printf("Triggering MDB reboot via Redis command")
		return u.redis.TriggerReboot()
	}

	// Use a more precise timer for the remaining time
	timer := time.NewTimer(remainingTime)
	defer timer.Stop()

	// Also poll every 30 seconds to verify state hasn't changed
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-u.ctx.Done():
			u.logger.Printf("MDB reboot cancelled due to context done.")
			return u.ctx.Err()
		case <-timer.C:
			// Final check before reboot
			currentState, err := u.redis.GetVehicleState(config.VehicleHashKey)
			if err != nil {
				u.logger.Printf("Failed to get vehicle state before reboot: %v. Proceeding anyway.", err)
			} else if currentState != "stand-by" {
				u.logger.Printf("Vehicle left 'stand-by' state (current: %s). Restarting wait.", currentState)
				return u.waitForStandbyWithSubscription(requiredDuration)
			}
			totalDuration := time.Since(standbyStartTime)
			u.logger.Printf("Vehicle has been in 'stand-by' for %v (since %s). Proceeding with MDB reboot.", totalDuration, standbyStartTime.Format(time.RFC3339))
			u.logger.Printf("Triggering MDB reboot via Redis command")
			return u.redis.TriggerReboot()
		case <-ticker.C:
			// Periodic check to ensure we're still in standby
			currentState, err := u.redis.GetVehicleState(config.VehicleHashKey)
			if err != nil {
				u.logger.Printf("Failed to get vehicle state during wait: %v. Continuing.", err)
				continue
			}
			if currentState != "stand-by" {
				u.logger.Printf("Vehicle left 'stand-by' state (current: %s). Restarting wait.", currentState)
				return u.waitForStandbyWithSubscription(requiredDuration)
			}
			durationInStandby := time.Since(standbyStartTime)
			remainingTime = requiredDuration - durationInStandby
			u.logger.Printf("Vehicle in 'stand-by' for %v. Waiting %v more.", durationInStandby, remainingTime)
		}
	}
}

// waitForStandbyWithSubscription waits for standby state using real-time subscription
func (u *Updater) waitForStandbyWithSubscription(requiredDuration time.Duration) error {
	// Try to subscribe to vehicle state changes
	stateChanges, cleanup, err := u.redis.SubscribeToVehicleStateChanges("vehicle:state:change")
	if err != nil {
		u.logger.Printf("Failed to subscribe to vehicle state changes: %v. Falling back to polling.", err)
		return u.waitForStandbyWithPolling(requiredDuration)
	}
	defer cleanup()

	// Also use a ticker as backup in case subscription misses events
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	var standbyStartTime time.Time

	for {
		select {
		case <-u.ctx.Done():
			u.logger.Printf("MDB reboot cancelled due to context done.")
			return u.ctx.Err()
		case <-stateChanges:
			// State changed, check current state
			currentState, stateTimestamp, err := u.redis.GetVehicleStateWithTimestamp(config.VehicleHashKey)
			if err != nil {
				u.logger.Printf("Failed to get vehicle state after change: %v. Continuing.", err)
				continue
			}

			if currentState == "stand-by" {
				if !stateTimestamp.IsZero() {
					standbyStartTime = stateTimestamp
				} else {
					standbyStartTime = time.Now()
				}
				u.logger.Printf("Vehicle entered 'stand-by' state at %s. Monitoring for %v.", standbyStartTime.Format(time.RFC3339), requiredDuration)

				// Check if already waited long enough
				durationInStandby := time.Since(standbyStartTime)
				if durationInStandby >= requiredDuration {
					u.logger.Printf("Vehicle has been in 'stand-by' for %v. Proceeding with MDB reboot.", durationInStandby)
					u.logger.Printf("Triggering MDB reboot via Redis command")
					return u.redis.TriggerReboot()
				}
				u.logger.Printf("Vehicle in 'stand-by' for %v. Waiting for %v.", durationInStandby, requiredDuration)
			} else {
				if !standbyStartTime.IsZero() {
					u.logger.Printf("Vehicle left 'stand-by' state (current: %s). Resetting standby timer.", currentState)
					standbyStartTime = time.Time{}
				} else {
					u.logger.Printf("Vehicle not in 'stand-by' (current: %s). Waiting.", currentState)
				}
			}
		case <-ticker.C:
			// Periodic check as backup
			currentState, stateTimestamp, err := u.redis.GetVehicleStateWithTimestamp(config.VehicleHashKey)
			if err != nil {
				u.logger.Printf("Failed to get vehicle state during periodic check: %v. Continuing.", err)
				continue
			}

			if currentState == "stand-by" {
				if standbyStartTime.IsZero() {
					if !stateTimestamp.IsZero() {
						standbyStartTime = stateTimestamp
					} else {
						standbyStartTime = time.Now()
					}
					u.logger.Printf("Vehicle entered 'stand-by' state at %s. Monitoring for %v.", standbyStartTime.Format(time.RFC3339), requiredDuration)
				}

				durationInStandby := time.Since(standbyStartTime)
				if durationInStandby >= requiredDuration {
					u.logger.Printf("Vehicle has been in 'stand-by' for %v (since %s). Proceeding with MDB reboot.", durationInStandby, standbyStartTime.Format(time.RFC3339))
					u.logger.Printf("Triggering MDB reboot via Redis command")
					return u.redis.TriggerReboot()
				}
				u.logger.Printf("Vehicle in 'stand-by' for %v. Waiting for %v.", durationInStandby, requiredDuration)
			} else {
				if !standbyStartTime.IsZero() {
					u.logger.Printf("Vehicle left 'stand-by' state (current: %s). Resetting standby timer.", currentState)
					standbyStartTime = time.Time{}
				}
			}
		}
	}
}

// waitForStandbyWithPolling is the fallback polling implementation
func (u *Updater) waitForStandbyWithPolling(requiredDuration time.Duration) error {
	var standbyStartTime time.Time
	// Use 30-second intervals instead of 1 minute for more responsive timing
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	// Check immediately first
	currentState, err := u.redis.GetVehicleState(config.VehicleHashKey)
	if err != nil {
		u.logger.Printf("Failed to get initial vehicle state: %v. Continuing with polling.", err)
	} else if currentState == "stand-by" {
		standbyStartTime = time.Now()
		u.logger.Printf("Vehicle entered 'stand-by' state at %s. Monitoring for %v.", standbyStartTime.Format(time.RFC3339), requiredDuration)
	}

	for {
		select {
		case <-u.ctx.Done():
			u.logger.Printf("MDB reboot cancelled due to context done.")
			return u.ctx.Err()
		case <-ticker.C:
			currentState, err := u.redis.GetVehicleState(config.VehicleHashKey)
			if err != nil {
				u.logger.Printf("Failed to get vehicle state for MDB reboot check: %v. Retrying.", err)
				standbyStartTime = time.Time{} // Reset timer on error
				continue
			}

			if currentState == "stand-by" {
				if standbyStartTime.IsZero() {
					standbyStartTime = time.Now()
					u.logger.Printf("Vehicle entered 'stand-by' state at %s. Monitoring for %v.", standbyStartTime.Format(time.RFC3339), requiredDuration)
				}
				durationInStandby := time.Since(standbyStartTime)
				if durationInStandby >= requiredDuration {
					u.logger.Printf("Vehicle has been in 'stand-by' for %v (since %s). Proceeding with MDB reboot.", durationInStandby, standbyStartTime.Format(time.RFC3339))
					u.logger.Printf("Triggering MDB reboot via Redis command")
					return u.redis.TriggerReboot()
				}
				u.logger.Printf("Vehicle in 'stand-by' for %v. Waiting for %v.", durationInStandby, requiredDuration)
			} else {
				if !standbyStartTime.IsZero() {
					u.logger.Printf("Vehicle left 'stand-by' state (current: %s). Resetting standby timer.", currentState)
					standbyStartTime = time.Time{}
				} else {
					u.logger.Printf("Vehicle not in 'stand-by' (current: %s). Waiting.", currentState)
				}
			}
		}
	}
}
