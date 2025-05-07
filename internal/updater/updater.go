package updater

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/librescoot/update-service/internal/config"
	"github.com/librescoot/update-service/internal/inhibitor"
	"github.com/librescoot/update-service/internal/redis"
	"github.com/librescoot/update-service/internal/vehicle"
)

// Updater represents the update orchestrator
type Updater struct {
	config      *config.Config
	redis       *redis.Client
	vehicle     *vehicle.Service
	inhibitor   *inhibitor.Client
	logger      *log.Logger
	ctx         context.Context
	cancel      context.CancelFunc
	githubAPI   *GitHubAPI
	updateState map[string]string
	stateMutex  sync.Mutex
	otaMessages <-chan string
	cleanupSub  func()
}

// New creates a new updater
func New(ctx context.Context, cfg *config.Config, redisClient *redis.Client, vehicleService *vehicle.Service, inhibitorClient *inhibitor.Client, logger *log.Logger) *Updater {
	updaterCtx, cancel := context.WithCancel(ctx)
	return &Updater{
		config:      cfg,
		redis:       redisClient,
		vehicle:     vehicleService,
		inhibitor:   inhibitorClient,
		logger:      logger,
		ctx:         updaterCtx,
		cancel:      cancel,
		githubAPI:   NewGitHubAPI(updaterCtx, cfg.GitHubReleasesURL),
		updateState: make(map[string]string),
		stateMutex:  sync.Mutex{},
	}
}

// Start starts the updater
func (u *Updater) Start() error {
	u.logger.Printf("Starting updater with check interval: %v", u.config.CheckInterval)

	// Subscribe to OTA status channel
	var err error
	u.otaMessages, u.cleanupSub, err = u.redis.SubscribeToOTAStatus(config.OtaChannel)
	if err != nil {
		return fmt.Errorf("failed to subscribe to OTA status channel: %w", err)
	}

	// Start the OTA status message handler
	go u.handleOTAStatusMessages()

	// Start the update check loop
	go u.updateCheckLoop()

	return nil
}

// handleOTAStatusMessages handles messages from the OTA status channel
func (u *Updater) handleOTAStatusMessages() {
	u.logger.Printf("Started OTA status message handler")

	for {
		select {
		case <-u.ctx.Done():
			u.logger.Printf("OTA status message handler stopped")
			return
		case msg, ok := <-u.otaMessages:
			if !ok {
				u.logger.Printf("OTA status channel closed")
				return
			}

			u.logger.Printf("Received OTA status message: %s", msg)

			// Handle different message types
			switch msg {
			case "status":
				// Get the current OTA status
				status, err := u.redis.GetOTAStatus(config.OtaStatusHashKey)
				if err != nil {
					u.logger.Printf("Failed to get OTA status: %v", err)
					continue
				}

				u.logger.Printf("Current OTA status: %v", status)

				// Process the status update for all tracked components
				u.processOTAStatus(status)

			case "update-type":
				// We can ignore update-type messages for now
				u.logger.Printf("Received update-type message, ignoring")

			default:
				u.logger.Printf("Unexpected OTA message: %s", msg)
			}
		}
	}
}

// processOTAStatus processes an OTA status update
func (u *Updater) processOTAStatus(status map[string]string) {
	// Check if we're tracking any components
	u.stateMutex.Lock()
	components := make([]string, 0, len(u.updateState))
	for component := range u.updateState {
		components = append(components, component)
	}
	u.stateMutex.Unlock()

	if len(components) == 0 {
		u.logger.Printf("No components being tracked, ignoring OTA status update")
		return
	}

	// Process the status for each tracked component
	for _, component := range components {
		u.processComponentStatus(component, status)
	}
}

// processComponentStatus processes an OTA status update for a specific component
func (u *Updater) processComponentStatus(component string, status map[string]string) {
	u.logger.Printf("Processing status update for component: %s", component)

	// Check if we're tracking this component
	u.stateMutex.Lock()
	_, tracking := u.updateState[component]
	u.stateMutex.Unlock()

	if !tracking {
		u.logger.Printf("Ignoring OTA status update for untracked component: %s", component)
		return
	}

	// Check status
	switch status["status"] {
	case "downloading":
		u.logger.Printf("Update downloading for %s", component)

		// Add download inhibit to delay power state changes
		if err := u.inhibitor.AddDownloadInhibit(component); err != nil {
			u.logger.Printf("Failed to add download inhibit for %s: %v", component, err)
		}

	case "installing":
		u.logger.Printf("Update installing for %s", component)

		// Remove download inhibit if it exists
		if err := u.inhibitor.RemoveDownloadInhibit(component); err != nil {
			u.logger.Printf("Failed to remove download inhibit for %s: %v", component, err)
		}

		// Add install inhibit to defer power state changes
		if err := u.inhibitor.AddInstallInhibit(component); err != nil {
			u.logger.Printf("Failed to add install inhibit for %s: %v", component, err)
		}

	case "complete", "installation-complete-waiting-reboot":
		isWaitingReboot := status["status"] == "installation-complete-waiting-reboot"

		if isWaitingReboot {
			u.logger.Printf("Update installed for %s, waiting for reboot", component)
		} else {
			u.logger.Printf("Update complete for %s", component)
		}

		u.stateMutex.Lock()
		u.updateState[component] = "complete"
		u.stateMutex.Unlock()

		// Remove inhibits
		if err := u.inhibitor.RemoveDownloadInhibit(component); err != nil {
			u.logger.Printf("Failed to remove download inhibit for %s: %v", component, err)
		}

		if err := u.inhibitor.RemoveInstallInhibit(component); err != nil {
			u.logger.Printf("Failed to remove install inhibit for %s: %v", component, err)
		}

		// Handle post-update actions
		if component == "dbc" {
			// Notify vehicle service that DBC update is complete
			if err := u.redis.PushUpdateCommand("complete-dbc"); err != nil {
				u.logger.Printf("Failed to send complete-dbc command: %v", err)
			}

			// If installation is complete and waiting for reboot, trigger reboot
			if isWaitingReboot {
				u.logger.Printf("Reboot needed for DBC")
				if err := u.vehicle.TriggerReboot("dbc"); err != nil {
					u.logger.Printf("Failed to trigger DBC reboot: %v", err)
				}
			}
		} else if component == "mdb" {
			// If installation is complete and waiting for reboot, trigger reboot
			if isWaitingReboot {
				u.logger.Printf("Reboot needed for MDB")

				// Check if it's safe to reboot
				safe, err := u.vehicle.IsSafeForMdbReboot()
				if err != nil {
					u.logger.Printf("Failed to check if safe for MDB reboot: %v", err)
					return
				}

				if safe {
					if err := u.vehicle.TriggerReboot("mdb"); err != nil {
						u.logger.Printf("Failed to trigger MDB reboot: %v", err)
					}
				} else {
					u.logger.Printf("Not safe to reboot MDB, scheduling reboot check")
					u.vehicle.ScheduleMdbRebootCheck(u.config.MdbRebootCheckInterval)
					go u.waitForMdbReboot()
				}
			}
		}

	case "failed":
		u.logger.Printf("Update failed for %s: %s", component, status["error"])
		u.stateMutex.Lock()
		u.updateState[component] = "failed"
		u.stateMutex.Unlock()

		// Remove inhibits
		if err := u.inhibitor.RemoveDownloadInhibit(component); err != nil {
			u.logger.Printf("Failed to remove download inhibit for %s: %v", component, err)
		}

		if err := u.inhibitor.RemoveInstallInhibit(component); err != nil {
			u.logger.Printf("Failed to remove install inhibit for %s: %v", component, err)
		}

		// Notify vehicle service that DBC update is complete if it was a DBC update
		if component == "dbc" {
			if err := u.redis.PushUpdateCommand("complete-dbc"); err != nil {
				u.logger.Printf("Failed to send complete-dbc command: %v", err)
			}
		}
	}
}

// Stop stops the updater
func (u *Updater) Stop() {
	// Clean up the subscription if it exists
	if u.cleanupSub != nil {
		u.cleanupSub()
	}

	u.cancel()
}

// hasUpdatesInProgress checks if any component updates are in progress
func (u *Updater) hasUpdatesInProgress() bool {
	u.stateMutex.Lock()
	defer u.stateMutex.Unlock()

	for _, state := range u.updateState {
		if state == "updating" {
			return true
		}
	}
	return false
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
			// Skip check if updates are in progress
			if u.hasUpdatesInProgress() {
				u.logger.Printf("Skipping update check because updates are in progress")
				continue
			}
			u.checkForUpdates()
		}
	}
}

// checkForUpdates checks for updates and initiates the update process if updates are available
func (u *Updater) checkForUpdates() {
	u.logger.Printf("Checking for updates")

	// Get releases from GitHub
	releases, err := u.githubAPI.GetReleases()
	if err != nil {
		u.logger.Printf("Failed to get releases: %v", err)
		return
	}

	u.logger.Printf("Found %d releases", len(releases))

	// Find available updates for each component
	type updateInfo struct {
		component string
		release   Release
		assetURL  string
	}
	var updates []updateInfo

	for _, component := range u.config.Components {
		// Get the latest release for the component and channel
		release, found := u.findLatestRelease(releases, component, u.config.DefaultChannel)
		if !found {
			u.logger.Printf("No release found for component %s and channel %s", component, u.config.DefaultChannel)
			continue
		}

		// Find the .mender asset for the component
		var menderAsset string
		var assetURL string
		for _, asset := range release.Assets {
			if strings.Contains(asset.Name, component) && strings.HasSuffix(asset.Name, ".mender") {
				menderAsset = asset.Name
				assetURL = asset.BrowserDownloadURL
				break
			}
		}

		u.logger.Printf("Found release for component %s: %s (asset: %s)", component, release.TagName, menderAsset)

		// Check if update is needed
		if u.isUpdateNeeded(component, release) {
			u.logger.Printf("Update needed for component %s", component)

			if assetURL != "" {
				updates = append(updates, updateInfo{
					component: component,
					release:   release,
					assetURL:  assetURL,
				})
			} else {
				u.logger.Printf("No .mender asset URL found for component %s", component)
			}
		} else {
			u.logger.Printf("No update needed for component %s", component)
		}
	}

	// Sequence updates: MDB first, then DBC
	if len(updates) > 0 {
		// Sort updates: MDB first, then DBC
		var mdbUpdate *updateInfo
		var dbcUpdate *updateInfo

		for i := range updates {
			if updates[i].component == "mdb" {
				mdbUpdate = &updates[i]
			} else if updates[i].component == "dbc" {
				dbcUpdate = &updates[i]
			}
		}

		// Apply MDB update first if available
		if mdbUpdate != nil {
			u.logger.Printf("Initiating MDB update first")
			if err := u.initiateUpdate(mdbUpdate.component, mdbUpdate.release); err != nil {
				u.logger.Printf("Failed to initiate MDB update: %v", err)
			}
		}

		// Apply DBC update if available and MDB update is not in progress
		if dbcUpdate != nil {
			if mdbUpdate != nil {
				u.logger.Printf("DBC update will be applied after MDB update completes")
				// TODO: Implement a mechanism to queue the DBC update after MDB completes
			} else {
				u.logger.Printf("Initiating DBC update")
				if err := u.initiateUpdate(dbcUpdate.component, dbcUpdate.release); err != nil {
					u.logger.Printf("Failed to initiate DBC update: %v", err)
				}
			}
		}
	}
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
			if strings.Contains(asset.Name, component) {
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

// isUpdateNeeded checks if an update is needed for the given component
func (u *Updater) isUpdateNeeded(component string, release Release) bool {
	// Get the currently installed version
	currentVersion, err := u.redis.GetComponentVersion(component)
	if err != nil {
		u.logger.Printf("Failed to get current %s version: %v", component, err)
		// If we can't get the current version, assume an update is needed
		return true
	}

	// If no version is installed, an update is needed
	if currentVersion == "" {
		u.logger.Printf("No %s version found, update needed", component)
		return true
	}

	// Extract the timestamp part from the release tag (format: nightly-20250506T214046)
	parts := strings.Split(release.TagName, "-")
	if len(parts) < 2 {
		u.logger.Printf("Invalid release tag format: %s", release.TagName)
		return true
	}

	// Convert to lowercase for comparison with Redis version_id
	normalizedReleaseVersion := strings.ToLower(parts[1])

	if currentVersion != normalizedReleaseVersion {
		u.logger.Printf("Update needed for %s: current=%s, release=%s", component, currentVersion, normalizedReleaseVersion)
		return true
	}

	u.logger.Printf("No update needed for %s: current=%s, release=%s", component, currentVersion, normalizedReleaseVersion)
	return false
}

// initiateUpdate initiates the update process for the given component
func (u *Updater) initiateUpdate(component string, release Release) error {
	// Find the .mender asset for the component
	var assetURL string
	for _, asset := range release.Assets {
		if strings.Contains(asset.Name, component) && strings.HasSuffix(asset.Name, ".mender") {
			assetURL = asset.BrowserDownloadURL
			break
		}
	}

	if assetURL == "" {
		return fmt.Errorf("no .mender asset found for component %s", component)
	}

	// Set update state
	u.stateMutex.Lock()
	u.updateState[component] = "updating"
	u.stateMutex.Unlock()

	// Handle component-specific update process
	switch component {
	case "dbc":
		return u.updateDBC(assetURL)
	case "mdb":
		return u.updateMDB(assetURL)
	default:
		return fmt.Errorf("unknown component: %s", component)
	}
}

// updateDBC updates the DBC component
func (u *Updater) updateDBC(assetURL string) error {
	// Check if it's safe to update DBC
	safe, err := u.vehicle.IsSafeForDbcUpdate()
	if err != nil {
		return fmt.Errorf("failed to check if safe for DBC update: %w", err)
	}

	if !safe {
		u.logger.Printf("Not safe to update DBC, scheduling retry")
		// This never actually happens, we just don't reboot the DBC
		return fmt.Errorf("not safe to update DBC")
	}

	// Notify vehicle service that DBC update is starting
	if err := u.redis.PushUpdateCommand("start-dbc"); err != nil {
		return fmt.Errorf("failed to send start-dbc command: %w", err)
	}

	// Push update URL to SMUT
	u.logger.Printf("Pushing DBC update URL to Redis key %s: %s", u.config.DbcUpdateKey, assetURL)
	if err := u.redis.PushUpdateURL(u.config.DbcUpdateKey, assetURL); err != nil {
		// Notify vehicle service that DBC update is complete (failed)
		u.redis.PushUpdateCommand("complete-dbc")
		return fmt.Errorf("failed to push DBC update URL: %w", err)
	}

	return nil
}

// updateMDB updates the MDB component
func (u *Updater) updateMDB(assetURL string) error {
	// Notify vehicle service that update is starting
	if err := u.redis.PushUpdateCommand("start"); err != nil {
		return fmt.Errorf("failed to send start command: %w", err)
	}

	// Push update URL to SMUT
	u.logger.Printf("Pushing MDB update URL to Redis key %s: %s", u.config.MdbUpdateKey, assetURL)
	if err := u.redis.PushUpdateURL(u.config.MdbUpdateKey, assetURL); err != nil {
		// Notify vehicle service that update is complete (failed)
		u.redis.PushUpdateCommand("complete")
		return fmt.Errorf("failed to push MDB update URL: %w", err)
	}

	return nil
}

// waitForMdbReboot waits for the MDB to be safe to reboot
func (u *Updater) waitForMdbReboot() {
	if u.vehicle.WaitForSafeReboot() {
		u.logger.Printf("Safe to reboot MDB now")
		if err := u.vehicle.TriggerReboot("mdb"); err != nil {
			u.logger.Printf("Failed to trigger MDB reboot: %v", err)
		}
	}
}
