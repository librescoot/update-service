package updater

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/librescoot/update-service/internal/config"
	"github.com/librescoot/update-service/internal/inhibitor"
	"github.com/librescoot/update-service/internal/mender"
	"github.com/librescoot/update-service/internal/redis"
	"github.com/librescoot/update-service/internal/status"
)

// Updater represents the component-aware update orchestrator
type Updater struct {
	config    *config.Config
	redis     *redis.Client // Client from internal/redis
	inhibitor *inhibitor.Client
	mender    *mender.Manager
	status    *status.Reporter
	githubAPI *GitHubAPI
	logger    *log.Logger
	ctx       context.Context
	cancel    context.CancelFunc
}

// New creates a new component-aware updater
func New(ctx context.Context, cfg *config.Config, redisClient *redis.Client, inhibitorClient *inhibitor.Client, logger *log.Logger) *Updater {
	updaterCtx, cancel := context.WithCancel(ctx)

	// Create download directory in /data/ota/{component}
	downloadDir := filepath.Join("/data/ota", cfg.Component)

	return &Updater{
		config:    cfg,
		redis:     redisClient,
		inhibitor: inhibitorClient,
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

	if err := u.inhibitor.AddDownloadInhibit(u.config.Component); err != nil {
		u.logger.Printf("Failed to add download inhibit: %v", err)
	}

	defer func() {
		// Always clean up inhibitors on exit
		if err := u.inhibitor.RemoveDownloadInhibit(u.config.Component); err != nil {
			u.logger.Printf("Failed to remove download inhibit: %v", err)
		}
		if err := u.inhibitor.RemoveInstallInhibit(u.config.Component); err != nil {
			u.logger.Printf("Failed to remove install inhibit: %v", err)
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

	// Step 5: Clean up the downloaded file
	if err := u.mender.CleanupFile(filePath); err != nil {
		u.logger.Printf("Failed to cleanup downloaded file: %v", err)
	}

	// Step 6: Set rebooting status and prepare for reboot
	if err := u.status.SetStatus(u.ctx, status.StatusRebooting); err != nil {
		u.logger.Printf("Failed to set rebooting status: %v", err)
	}

	// Remove install inhibitor before reboot
	if err := u.inhibitor.RemoveInstallInhibit(u.config.Component); err != nil {
		u.logger.Printf("Failed to remove install inhibit: %v", err)
	}

	// Step 7: Trigger reboot (component will reboot automatically or system will reboot)
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
		var standbyStartTime time.Time
		const requiredStandbyDuration = 3 * time.Minute
		ticker := time.NewTicker(15 * time.Second) // Check every 15 seconds
		defer ticker.Stop()

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
						u.logger.Printf("Vehicle entered 'stand-by' state at %s. Monitoring for %v.", standbyStartTime.Format(time.RFC3339), requiredStandbyDuration)
					}
					durationInStandby := time.Since(standbyStartTime)
					if durationInStandby >= requiredStandbyDuration {
						u.logger.Printf("Vehicle has been in 'stand-by' for %v (since %s). Proceeding with MDB reboot.", durationInStandby, standbyStartTime.Format(time.RFC3339))
						u.logger.Printf("Triggering MDB reboot via Redis command")
						return u.redis.TriggerReboot()
					}
					u.logger.Printf("Vehicle in 'stand-by' for %v. Waiting for %v.", durationInStandby, requiredStandbyDuration)
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

	case "dbc":
		safe, err := u.IsSafeForDbcReboot()
		if err != nil {
			return fmt.Errorf("failed to check DBC reboot safety: %w", err)
		}
		if !safe {
			currentState, _ := u.redis.GetVehicleState(config.VehicleHashKey)
			return fmt.Errorf("not safe to reboot DBC in current state: %s", currentState)
		}
		u.logger.Printf("Triggering DBC restart via Redis commands (complete-dbc, start-dbc)")
		if err := u.redis.PushUpdateCommand("complete-dbc"); err != nil {
			return fmt.Errorf("failed to send complete-dbc command: %w", err)
		}
		time.Sleep(500 * time.Millisecond) // Allow time for complete-dbc to be processed
		if err := u.redis.PushUpdateCommand("start-dbc"); err != nil {
			return fmt.Errorf("failed to send start-dbc command: %w", err)
		}
		return nil

	default:
		return fmt.Errorf("unknown component for reboot: %s", component)
	}
}
