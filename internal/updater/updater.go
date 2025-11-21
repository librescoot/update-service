package updater

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	config           *config.Config
	redis            *redis.Client // Client from internal/redis
	inhibitor        *inhibitor.Client
	power            *power.Client
	mender           *mender.Manager
	status           *status.Reporter
	githubAPI        *GitHubAPI
	logger           *log.Logger
	ctx              context.Context
	cancel           context.CancelFunc
	standbyStartTime time.Time // Tracks when vehicle entered standby state

	// Update concurrency control
	updateMu sync.Mutex // Prevents concurrent update checks during long-running multi-delta operations

	// Update method configuration
	updateMethodMu sync.RWMutex
	updateMethod   string
}

// New creates a new component-aware updater
func New(ctx context.Context, cfg *config.Config, redisClient *redis.Client, inhibitorClient *inhibitor.Client, powerClient *power.Client, logger *log.Logger) *Updater {
	updaterCtx, cancel := context.WithCancel(ctx)

	// Determine download directory
	downloadDir := cfg.DownloadDir
	if downloadDir == "" {
		downloadDir = filepath.Join("/data/ota", cfg.Component)
	}

	u := &Updater{
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

	// Initialize update method from Redis
	updateMethod, err := redisClient.GetUpdateMethod(cfg.Component)
	if err != nil {
		logger.Printf("Failed to get initial update method for %s: %v (defaulting to full)", cfg.Component, err)
		updateMethod = "full"
	}
	u.updateMethod = updateMethod
	logger.Printf("Initialized update method for %s: %s", cfg.Component, updateMethod)

	return u
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

	// Initialize Redis keys if they don't exist
	if err := u.initializeRedisKeys(); err != nil {
		u.logger.Printf("Warning: Failed to initialize Redis keys: %v", err)
	}

	// Check initial vehicle state and set standby timestamp if needed
	u.checkInitialStandbyState()

	// Start monitoring for settings changes
	go u.monitorSettingsChanges()

	// Start listening for Redis commands
	go u.listenForCommands()

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

// getUpdateMethod returns the current update method (thread-safe)
func (u *Updater) getUpdateMethod() string {
	u.updateMethodMu.RLock()
	defer u.updateMethodMu.RUnlock()
	return u.updateMethod
}

// setUpdateMethod sets the current update method (thread-safe)
func (u *Updater) setUpdateMethod(method string) {
	u.updateMethodMu.Lock()
	defer u.updateMethodMu.Unlock()
	if u.updateMethod != method {
		u.logger.Printf("Update method for %s changed from %s to %s", u.config.Component, u.updateMethod, method)
		u.updateMethod = method
	}
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

		// For DBC component, notify vehicle-service that update is complete
		// (the defer that normally sends this didn't execute due to reboot)
		if u.config.Component == "dbc" {
			u.logger.Printf("Sending complete-dbc command after reboot")
			if err := u.redis.PushUpdateCommand("complete-dbc"); err != nil {
				u.logger.Printf("Failed to send complete-dbc command: %v", err)
			}
		}
	}

	return nil
}

// initializeRedisKeys ensures Redis keys for this component are initialized
func (u *Updater) initializeRedisKeys() error {
	// Check if status key exists and get its value
	statusKey := fmt.Sprintf("status:%s", u.config.Component)
	statusValue, err := u.redis.GetClient().HGet(u.ctx, "ota", statusKey).Result()

	// Initialize if key doesn't exist (redis.Nil) or if it's empty
	needsInit := err != nil || statusValue == ""

	if needsInit {
		u.logger.Printf("Initializing Redis OTA keys for component %s", u.config.Component)

		// Get the configured update method
		updateMethod := u.getUpdateMethod()

		// Initialize keys: status=idle, download-progress=0, update-method=<configured>
		if err := u.status.Initialize(u.ctx, updateMethod); err != nil {
			return fmt.Errorf("failed to initialize OTA keys: %w", err)
		}
	}

	return nil
}

// checkInitialStandbyState checks the initial vehicle state on startup and sets standby timestamp
func (u *Updater) checkInitialStandbyState() {
	currentState, stateTimestamp, err := u.redis.GetVehicleStateWithTimestamp(config.VehicleHashKey)
	if err != nil {
		u.logger.Printf("Failed to get initial vehicle state: %v", err)
		return
	}

	u.logger.Printf("Checking initial vehicle state for MDB reboot optimization...")

	if currentState == "stand-by" {
		if !stateTimestamp.IsZero() {
			u.standbyStartTime = stateTimestamp
			elapsed := time.Since(stateTimestamp)
			u.logger.Printf("Vehicle in 'stand-by' since %s (elapsed: %v) - MDB reboot will use this timestamp", stateTimestamp.Format(time.RFC3339), elapsed)
		} else {
			u.standbyStartTime = time.Now()
			u.logger.Printf("Vehicle in 'stand-by' with no timestamp - using current time for MDB reboot tracking")
		}
	} else {
		u.logger.Printf("Vehicle not in 'stand-by' (current: %s) - MDB reboot will wait for standby state", currentState)
	}
}

// revalidateStandbyState re-validates the vehicle state after long-running operations
// This ensures the standby timestamp reflects the current vehicle state, not a stale
// timestamp from before the operation started. This is critical after operations like
// delta patch application which can take 20+ minutes during which the vehicle state
// may have changed multiple times.
func (u *Updater) revalidateStandbyState() {
	currentState, stateTimestamp, err := u.redis.GetVehicleStateWithTimestamp(config.VehicleHashKey)
	if err != nil {
		u.logger.Printf("Failed to get vehicle state after long-running operation: %v (clearing standby timestamp)", err)
		u.standbyStartTime = time.Time{} // Clear on error to be safe
		return
	}

	if currentState == "stand-by" {
		if !stateTimestamp.IsZero() {
			u.standbyStartTime = stateTimestamp
			elapsed := time.Since(stateTimestamp)
			u.logger.Printf("Vehicle in 'stand-by' after operation completion (timestamp: %s, elapsed: %v) - standby timer reset", stateTimestamp.Format(time.RFC3339), elapsed)
		} else {
			u.standbyStartTime = time.Now()
			u.logger.Printf("Vehicle in 'stand-by' after operation completion (no timestamp) - using current time for standby tracking")
		}
	} else {
		u.logger.Printf("Vehicle not in 'stand-by' after operation completion (current: %s) - clearing standby timestamp", currentState)
		u.standbyStartTime = time.Time{}
	}
}

// listenForCommands listens for Redis commands on scooter:update:{component}
func (u *Updater) listenForCommands() {
	channel := fmt.Sprintf("scooter:update:%s", u.config.Component)
	u.logger.Printf("Starting update command listener on %s", channel)

	for {
		select {
		case <-u.ctx.Done():
			u.logger.Printf("Update command listener stopped")
			return

		default:
			// Use BRPOP with 5 second timeout to allow periodic context checks
			result, err := u.redis.GetClient().BRPop(u.ctx, 5*time.Second, channel).Result()
			if err != nil {
				// Ignore timeout and context cancellation errors
				if err.Error() == "redis: nil" || err == context.Canceled {
					continue
				}
				u.logger.Printf("Error reading from %s: %v", channel, err)
				continue
			}

			// BRPOP returns [key, value]
			if len(result) >= 2 {
				command := result[1]
				u.logger.Printf("Received update command: %s", command)
				u.handleCommand(command)
			}
		}
	}
}

// handleCommand handles incoming Redis commands
func (u *Updater) handleCommand(command string) {
	switch command {
	case "check-now":
		u.logger.Printf("Received check-now command, triggering immediate update check")
		go u.checkForUpdates()
	default:
		u.logger.Printf("Unknown update command: %s", command)
	}
}

// monitorSettingsChanges monitors Redis pub/sub for update method configuration changes
func (u *Updater) monitorSettingsChanges() {
	// Subscribe to the general settings channel
	channel := "settings"
	u.logger.Printf("Subscribing to settings changes on channel: %s", channel)

	settingsChanges, cleanup, err := u.redis.SubscribeToSettingsChanges(channel)
	if err != nil {
		u.logger.Printf("Failed to subscribe to settings changes: %v", err)
		return
	}
	defer cleanup()

	// The setting key we're interested in for this component
	settingKey := fmt.Sprintf("updates.%s.method", u.config.Component)
	u.logger.Printf("Successfully subscribed to settings changes, monitoring for key: %s", settingKey)

	for {
		select {
		case <-u.ctx.Done():
			u.logger.Printf("Settings monitor stopped for %s", u.config.Component)
			return
		case msg, ok := <-settingsChanges:
			if !ok {
				u.logger.Printf("Settings changes channel closed for %s", u.config.Component)
				return
			}

			// Check if this message is for our component's update method setting
			if msg != settingKey {
				// Not our setting, ignore
				continue
			}

			// When we receive a notification for our setting, fetch the new value from Redis
			u.logger.Printf("Received settings change notification for %s", settingKey)

			newMethod, err := u.redis.GetUpdateMethod(u.config.Component)
			if err != nil {
				u.logger.Printf("Failed to get updated method for %s: %v", u.config.Component, err)
				continue
			}

			// Update the cached value
			u.setUpdateMethod(newMethod)
		}
	}
}

// updateCheckLoop periodically checks for updates
func (u *Updater) updateCheckLoop() {
	// If check interval is 0, automated updates are disabled
	if u.config.CheckInterval == 0 {
		u.logger.Printf("Automated update checks disabled (check-interval is 0 or 'never')")
		// Wait for context cancellation
		<-u.ctx.Done()
		u.logger.Printf("Update check loop stopped")
		return
	}

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

	// Prevent concurrent update checks during long-running multi-delta operations
	if !u.updateMu.TryLock() {
		u.logger.Printf("Update already in progress for %s, skipping this check", u.config.Component)
		return
	}
	defer u.updateMu.Unlock()

	// Store the timestamp of this check
	if err := u.redis.SetLastUpdateCheckTime(u.config.Component, time.Now()); err != nil {
		u.logger.Printf("Warning: Failed to store last check time: %v", err)
	}

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

	// Get the currently installed version
	currentVersion, err := u.getCurrentVersion()
	if err != nil {
		u.logger.Printf("Failed to get current %s version: %v", u.config.Component, err)
		// Continue anyway - we might not have a version installed yet
		currentVersion = ""
	}

	// Get releases from GitHub
	releases, err := u.githubAPI.GetReleases()
	if err != nil {
		u.logger.Printf("Failed to get releases: %v", err)
		return
	}

	// Get the variant_id to find releases for the correct variant
	variantID, err := u.redis.GetVariantID(u.config.Component)
	if err != nil {
		u.logger.Printf("Failed to get variant_id for component %s: %v (falling back to component name)", u.config.Component, err)
		variantID = u.config.Component
	}

	// Get the cached update method
	updateMethod := u.getUpdateMethod()
	u.logger.Printf("Update method for %s: %s", u.config.Component, updateMethod)

	// Check for channel switch
	if currentVersion != "" {
		currentChannel := u.inferChannelFromVersion(currentVersion)
		if currentChannel != "" && currentChannel != u.config.Channel {
			u.logger.Printf("Channel switch detected from %s to %s for component %s. Forcing full update.", currentChannel, u.config.Channel, u.config.Component)
			updateMethod = "full"
		}
	}

	// If delta updates are configured and we have a current version
	if updateMethod == "delta" && currentVersion != "" {
		// Attempt delta update
		go u.performDeltaUpdate(releases, currentVersion, variantID)
		return
	}

	// Otherwise, use full update (either because full is configured or delta prerequisites not met)
	if updateMethod == "delta" && currentVersion == "" {
		u.logger.Printf("No current version found, using full update for initial installation")
	}

	// Find the latest release for our variant and channel
	release, found := u.findLatestRelease(releases, variantID, u.config.Channel)
	if !found {
		u.logger.Printf("No release found for variant_id %s and channel %s", variantID, u.config.Channel)
		return
	}

	u.logger.Printf("Found release %s, looking for asset matching variant_id: %s", release.TagName, variantID)

	// Find the .mender asset for the variant
	var assetURL string
	for _, asset := range release.Assets {
		// Match assets by variant_id (e.g., "unu-mdb", "unu-dbc", "rpi5")
		// Asset names should be like: librescoot-{variant_id}-{timestamp}.mender
		if strings.Contains(asset.Name, variantID) && strings.HasSuffix(asset.Name, ".mender") {
			assetURL = asset.BrowserDownloadURL
			u.logger.Printf("Found matching asset: %s", asset.Name)
			break
		}
	}

	if assetURL == "" {
		u.logger.Printf("No .mender asset found for variant_id %s in release %s", variantID, release.TagName)
		return
	}

	// Check if update is needed
	if !u.isUpdateNeeded(release) {
		u.logger.Printf("No update needed for component %s", u.config.Component)
		return
	}

	u.logger.Printf("Update needed for %s: %s (using full update)", u.config.Component, release.TagName)

	// Start the update process
	go u.performUpdate(release, assetURL)
}

// inferChannelFromVersion attempts to infer the channel from the version string
func (u *Updater) inferChannelFromVersion(version string) string {
	// Clean up version string (remove potential codename suffix like " (none)")
	version = strings.Split(version, " ")[0]

	if strings.HasPrefix(version, "nightly-") {
		return "nightly"
	}
	if strings.HasPrefix(version, "testing-") {
		return "testing"
	}
	if strings.HasPrefix(version, "v") || (len(version) > 0 && version[0] >= '0' && version[0] <= '9') {
		// Starts with 'v' or a number, assume stable
		return "stable"
	}
	return ""
}

// findLatestRelease finds the latest release for the given variant and channel
func (u *Updater) findLatestRelease(releases []Release, variantID, channel string) (Release, bool) {
	var latestRelease Release
	found := false

	for _, release := range releases {
		// Channel-specific filtering logic
		match := false
		switch channel {
		case "nightly":
			// Nightly: look for prereleases with "nightly-" prefix
			if release.Prerelease && strings.HasPrefix(release.TagName, "nightly-") {
				match = true
			}
		case "testing":
			// Testing: look for prereleases with "testing-" prefix
			if release.Prerelease && strings.HasPrefix(release.TagName, "testing-") {
				match = true
			}
		case "stable":
			// Stable: look for non-prereleases with "v" prefix (e.g., v1.2.3)
			if !release.Prerelease && strings.HasPrefix(release.TagName, "v") {
				match = true
			}
		default:
			// Fallback for unknown channels (legacy behavior: match channel prefix)
			if strings.HasPrefix(release.TagName, channel+"-") {
				match = true
			}
		}

		if !match {
			continue
		}

		// Check if the release has assets for the specified variant
		hasVariantAsset := false
		for _, asset := range release.Assets {
			if strings.Contains(asset.Name, variantID) && strings.HasSuffix(asset.Name, ".mender") {
				hasVariantAsset = true
				break
			}
		}

		if !hasVariantAsset {
			continue
		}

		// Comparison logic
		if !found {
			latestRelease = release
			found = true
			continue
		}

		// For stable, use semantic version comparison
		if channel == "stable" {
			if compareVersions(release.TagName, latestRelease.TagName) > 0 {
				latestRelease = release
			}
		} else {
			// For nightly/testing, rely on PublishedAt
			if release.PublishedAt.After(latestRelease.PublishedAt) {
				latestRelease = release
			}
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

	// Handle stable channel version comparison (vX.Y.Z)
	if u.config.Channel == "stable" {
		// Current version might be just "1.2.3" or "v1.2.3", release tag is "v1.2.3"
		// Normalize both to ensure comparison works
		normCurrent := currentVersion
		if !strings.HasPrefix(normCurrent, "v") {
			normCurrent = "v" + normCurrent
		}

		if compareVersions(release.TagName, normCurrent) > 0 {
			u.logger.Printf("Update needed for %s (stable): current=%s, release=%s", u.config.Component, currentVersion, release.TagName)
			return true
		}

		u.logger.Printf("No update needed for %s (stable): current=%s, release=%s", u.config.Component, currentVersion, release.TagName)
		return false
	}

	// Handle nightly/testing channels (timestamp based: channel-YYYYMMDD...)
	// Handle nightly/testing channels (timestamp based: channel-YYYYMMDD...)
	normalizedReleaseVersion := strings.ToLower(release.TagName)
	normalizedCurrentVersion := strings.ToLower(currentVersion)

	// If current version is short (legacy), try to match it against the short part of release
	if !strings.HasPrefix(normalizedCurrentVersion, u.config.Channel+"-") {
		parts := strings.Split(normalizedReleaseVersion, "-")
		if len(parts) >= 2 && normalizedCurrentVersion == parts[1] {
			u.logger.Printf("No update needed for %s: current=%s (legacy), release=%s", u.config.Component, currentVersion, normalizedReleaseVersion)
			return false
		}
	}

	if normalizedCurrentVersion != normalizedReleaseVersion {
		u.logger.Printf("Update needed for %s: current=%s, release=%s", u.config.Component, currentVersion, normalizedReleaseVersion)
		return true
	}

	u.logger.Printf("No update needed for %s: current=%s, release=%s", u.config.Component, currentVersion, normalizedReleaseVersion)
	return false
}

// compareVersions compares two version strings (v1, v2).
// Returns 1 if v1 > v2, -1 if v1 < v2, 0 if equal.
// Assumes format vX.Y.Z or X.Y.Z
func compareVersions(v1, v2 string) int {
	v1 = strings.TrimPrefix(v1, "v")
	v2 = strings.TrimPrefix(v2, "v")

	parts1 := strings.Split(v1, ".")
	parts2 := strings.Split(v2, ".")

	maxLen := len(parts1)
	if len(parts2) > maxLen {
		maxLen = len(parts2)
	}

	for i := 0; i < maxLen; i++ {
		var n1, n2 int
		var err error

		if i < len(parts1) {
			n1, err = strconv.Atoi(parts1[i])
			if err != nil {
				// If not a number, treat as 0 or handle differently if needed
				// For now, simple integer comparison
				n1 = 0
			}
		}

		if i < len(parts2) {
			n2, err = strconv.Atoi(parts2[i])
			if err != nil {
				n2 = 0
			}
		}

		if n1 > n2 {
			return 1
		}
		if n1 < n2 {
			return -1
		}
	}

	return 0
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

	var version string
	if u.config.Channel == "stable" {
		version = release.TagName
	} else {
		// Use full tag name for nightly/testing too
		version = strings.ToLower(release.TagName)
	}

	// Step 0: Check and commit any pending Mender update
	u.logger.Printf("Checking and attempting to commit any pending Mender update for %s before starting new update to %s", u.config.Component, release.TagName)
	// Attempt to commit. Log outcome (mender.Commit logs details), but don't let failure here stop the new update.
	if errCommit := u.mender.Commit(); errCommit != nil {
		u.logger.Printf("Attempt to commit pending Mender update for %s before new update to %s resulted in an error (proceeding with new update): %v", u.config.Component, release.TagName, errCommit)
	} else {
		u.logger.Printf("Attempt to commit pending Mender update for %s before new update to %s completed.", u.config.Component, release.TagName)
	}
	u.logger.Printf("Proceeding with update to %s for component %s.", release.TagName, u.config.Component)

	// Step 1: Set downloading status, update method, and add download inhibitor
	if err := u.status.SetStatusAndVersion(u.ctx, status.StatusDownloading, version); err != nil {
		u.logger.Printf("Failed to set downloading status: %v", err)
		return
	}

	// Set update method to full
	if err := u.status.SetUpdateMethod(u.ctx, "full"); err != nil {
		u.logger.Printf("Failed to set update method: %v", err)
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
	progressCallback := func(downloaded, total int64) {
		if err := u.status.SetDownloadProgress(u.ctx, downloaded, total); err != nil {
			u.logger.Printf("Failed to update download progress: %v", err)
		}
	}

	filePath, err := u.mender.DownloadAndVerify(u.ctx, assetURL, "", progressCallback)
	if err != nil {
		u.logger.Printf("Failed to download update: %v", err)
		if err := u.status.SetError(u.ctx, "download-failed", fmt.Sprintf("Failed to download update: %v", err)); err != nil {
			u.logger.Printf("Failed to set error status: %v", err)
		}
		return
	}

	// Clear download progress after successful download
	if err := u.status.ClearDownloadProgress(u.ctx); err != nil {
		u.logger.Printf("Failed to clear download progress: %v", err)
	}

	u.logger.Printf("Successfully downloaded update to: %s", filePath)

	// Re-validate vehicle state after long-running download operation
	// This ensures the 3-minute standby requirement starts fresh from the current state
	u.revalidateStandbyState()

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
		if err := u.status.SetError(u.ctx, "install-failed", fmt.Sprintf("Failed to install update: %v", err)); err != nil {
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
				if statusErr := u.status.SetError(u.ctx, "reboot-failed", fmt.Sprintf("Failed to trigger %s reboot: %v", u.config.Component, err)); statusErr != nil {
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

// performDeltaUpdate attempts to apply a chain of delta updates to reach the latest version
func (u *Updater) performDeltaUpdate(releases []Release, currentVersion, variantID string) {
	// Step 1: Build the delta chain
	deltaChain, err := u.buildDeltaChain(releases, currentVersion, u.config.Channel, variantID)
	if err != nil {
		u.logger.Printf("Failed to build delta chain: %v", err)

		// Fall back to full update
		latestRelease, found := u.findLatestRelease(releases, variantID, u.config.Channel)
		if found {
			menderURL := u.findMenderAsset(latestRelease, variantID)
			if menderURL != "" {
				u.logger.Printf("Falling back to full update with latest version")
				u.performUpdate(latestRelease, menderURL)
			}
		}
		return
	}

	if deltaChain == nil || len(deltaChain) == 0 {
		u.logger.Printf("Already at latest version %s", currentVersion)
		return
	}

	// Get latest version from chain
	latestVersion := strings.ToLower(deltaChain[len(deltaChain)-1].TagName)

	// Log the delta chain
	u.logger.Printf("Built delta chain with %d updates: %s -> %s", len(deltaChain), currentVersion, latestVersion)
	for i, release := range deltaChain {
		u.logger.Printf("  Delta %d/%d: %s", i+1, len(deltaChain), release.TagName)
	}
	u.logger.Printf("Starting multi-delta update process for %s: %s -> %s (%d deltas)", u.config.Component, currentVersion, latestVersion, len(deltaChain))

	// Commit any pending update before starting
	u.logger.Printf("Checking and attempting to commit any pending Mender update for %s before starting delta update to %s", u.config.Component, latestVersion)
	if err := u.mender.Commit(); err != nil {
		u.logger.Printf("Attempt to commit pending Mender update for %s before delta update to %s resulted in an error (proceeding with delta update): %v", u.config.Component, latestVersion, err)
	}

	// Set status
	if err := u.status.SetStatusAndVersion(u.ctx, status.StatusDownloading, latestVersion); err != nil {
		u.logger.Printf("Failed to set downloading status: %v", err)
	}

	// Set update method
	if err := u.status.SetUpdateMethod(u.ctx, "delta"); err != nil {
		u.logger.Printf("Failed to set update method: %v", err)
	}

	// For DBC updates, notify vehicle-service to keep dashboard power on
	if u.config.Component == "dbc" {
		u.logger.Printf("Starting DBC multi-delta update - sending start-dbc command")
		if err := u.redis.PushUpdateCommand("start-dbc"); err != nil {
			u.logger.Printf("Failed to send start-dbc command: %v", err)
		}
	}

	// Add power inhibit
	if err := u.inhibitor.AddDownloadInhibit(u.config.Component); err != nil {
		u.logger.Printf("Failed to add download inhibit: %v", err)
	}

	// Request ondemand CPU governor
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

	// Progress callbacks
	downloadProgressCallback := func(downloaded, total int64) {
		if err := u.status.SetDownloadProgress(u.ctx, downloaded, total); err != nil {
			u.logger.Printf("Failed to set download progress: %v", err)
		}
	}

	installProgressCallback := func(percent int) {
		if err := u.status.SetInstallProgress(u.ctx, percent); err != nil {
			u.logger.Printf("Failed to set install progress: %v", err)
		}
	}

	// Step 2: Apply all deltas in the chain sequentially
	var finalMenderPath string
	workingVersion := currentVersion

	for deltaIndex, release := range deltaChain {
		deltaNum := deltaIndex + 1
		u.logger.Printf("=== Applying delta %d/%d: %s ===", deltaNum, len(deltaChain), release.TagName)

		// Find delta URL for this release
		deltaURL := ""
		for _, asset := range release.Assets {
			if strings.Contains(asset.Name, variantID) && strings.HasSuffix(asset.Name, ".delta") {
				u.logger.Printf("Found delta asset: %s", asset.Name)
				deltaURL = asset.BrowserDownloadURL
				break
			}
		}

		if deltaURL == "" {
			u.logger.Printf("No delta asset found for %s, cannot continue multi-delta chain", release.TagName)
			if err := u.status.SetError(u.ctx, "delta-failed", fmt.Sprintf("Delta %d/%d failed: no delta asset found", deltaNum, len(deltaChain))); err != nil {
				u.logger.Printf("Failed to set error status: %v", err)
			}

			// Clear error and fall back to full update
			time.Sleep(2 * time.Second)
			if err := u.status.SetIdleAndClearVersion(u.ctx); err != nil {
				u.logger.Printf("Failed to clear status before fallback: %v", err)
			}

			latestRelease, found := u.findLatestRelease(releases, variantID, u.config.Channel)
			if found {
				menderURL := u.findMenderAsset(latestRelease, variantID)
				if menderURL != "" {
					u.logger.Printf("Falling back to full update with latest version")
					u.performUpdate(latestRelease, menderURL)
				}
			}
			return
		}

		targetVersion := strings.ToLower(release.TagName)

		u.logger.Printf("Downloading and applying delta %d/%d: %s -> %s", deltaNum, len(deltaChain), workingVersion, targetVersion)

		// Apply this delta
		newMenderPath, err := u.mender.ApplyDeltaUpdate(u.ctx, deltaURL, workingVersion, downloadProgressCallback, installProgressCallback)
		if err != nil {
			u.logger.Printf("Delta %d/%d failed: %v", deltaNum, len(deltaChain), err)
			if err := u.status.SetError(u.ctx, "delta-failed", fmt.Sprintf("Delta %d/%d failed: %v", deltaNum, len(deltaChain), err)); err != nil {
				u.logger.Printf("Failed to set error status: %v", err)
			}

			// Clear error and fall back to full update
			time.Sleep(2 * time.Second)
			if err := u.status.SetIdleAndClearVersion(u.ctx); err != nil {
				u.logger.Printf("Failed to clear status before fallback: %v", err)
			}

			latestRelease, found := u.findLatestRelease(releases, variantID, u.config.Channel)
			if found {
				menderURL := u.findMenderAsset(latestRelease, variantID)
				if menderURL != "" {
					u.logger.Printf("Falling back to full update with latest version")
					u.performUpdate(latestRelease, menderURL)
				}
			}
			return
		}

		u.logger.Printf("Delta %d/%d applied successfully: %s", deltaNum, len(deltaChain), newMenderPath)

		// Clear progress for this delta
		if err := u.status.ClearDownloadProgress(u.ctx); err != nil {
			u.logger.Printf("Failed to clear download progress: %v", err)
		}
		if err := u.status.ClearInstallProgress(u.ctx); err != nil {
			u.logger.Printf("Failed to clear install progress: %v", err)
		}

		// Update working version and final path for next iteration
		workingVersion = targetVersion
		finalMenderPath = newMenderPath
	}

	u.logger.Printf("All deltas applied successfully, final mender file: %s", finalMenderPath)

	// Re-validate vehicle state after long-running delta patch operation
	// This ensures the 3-minute standby requirement starts fresh from the current state
	u.revalidateStandbyState()

	// If dry-run mode, stop here - don't install or reboot
	if u.config.DryRun {
		u.logger.Printf("DRY-RUN: Multi-delta update complete. Final mender file ready at: %s", finalMenderPath)
		u.logger.Printf("DRY-RUN: Skipping mender-update install and reboot")
		if err := u.status.SetIdleAndClearVersion(u.ctx); err != nil {
			u.logger.Printf("Failed to set idle status in dry run: %v", err)
		}
		return
	}

	// Step 3: Set installing status and add install inhibitor
	if err := u.status.SetStatus(u.ctx, status.StatusInstalling); err != nil {
		u.logger.Printf("Failed to set installing status: %v", err)
		return
	}

	// Add install inhibit before removing download inhibit
	if err := u.inhibitor.AddInstallInhibit(u.config.Component); err != nil {
		u.logger.Printf("Failed to add install inhibit: %v", err)
	}

	if err := u.inhibitor.RemoveDownloadInhibit(u.config.Component); err != nil {
		u.logger.Printf("Failed to remove download inhibit: %v", err)
	}

	// Step 4: Install the update
	if err := u.mender.Install(finalMenderPath); err != nil {
		u.logger.Printf("Failed to install delta-generated update: %v", err)
		if err := u.status.SetError(u.ctx, "install-failed", fmt.Sprintf("Failed to install delta-generated update: %v", err)); err != nil {
			u.logger.Printf("Failed to set error status: %v", err)
		}
		return
	}

	u.logger.Printf("Successfully installed delta update")

	// Step 5: Set rebooting status and prepare for reboot
	if err := u.status.SetStatus(u.ctx, status.StatusRebooting); err != nil {
		u.logger.Printf("Failed to set rebooting status: %v", err)
	}

	// Remove install inhibitor before reboot
	if err := u.inhibitor.RemoveInstallInhibit(u.config.Component); err != nil {
		u.logger.Printf("Failed to remove install inhibit: %v", err)
	}

	// Step 6: Trigger reboot
	u.logger.Printf("Delta update installation complete, system will reboot to apply changes")

	// Trigger reboot
	if u.config.Component == "mdb" || u.config.Component == "dbc" {
		u.logger.Printf("%s delta update installed, triggering reboot", strings.ToUpper(u.config.Component))
		err := u.TriggerReboot(u.config.Component)
		if err != nil {
			u.logger.Printf("Failed to trigger %s reboot: %v", u.config.Component, err)
			if !strings.Contains(err.Error(), "DRY-RUN") {
				if statusErr := u.status.SetError(u.ctx, "reboot-failed", fmt.Sprintf("Failed to trigger %s reboot: %v", u.config.Component, err)); statusErr != nil {
					u.logger.Printf("Additionally failed to set error status after %s reboot trigger failure: %v", u.config.Component, statusErr)
				}
			}

			// If it was a dry run, simulate post-reboot
			if u.config.DryRun || strings.Contains(err.Error(), "DRY-RUN") {
				u.logger.Printf("Dry run or simulated reboot: Simulating post-reboot state by setting idle status for %s.", u.config.Component)
				if idleErr := u.status.SetIdleAndClearVersion(u.ctx); idleErr != nil {
					u.logger.Printf("Failed to set idle status in dry run for %s: %v", u.config.Component, idleErr)
				}
			}
		}
	} else {
		u.logger.Printf("Unknown component %s, cannot determine reboot strategy. Setting to idle.", u.config.Component)
		if err := u.status.SetIdleAndClearVersion(u.ctx); err != nil {
			u.logger.Printf("Failed to set idle status for unknown component: %v", err)
		}
	}
}

// TriggerReboot triggers a reboot or restart of the specified component.
func (u *Updater) TriggerReboot(component string) error {
	if u.config.DryRun {
		u.logger.Printf("DRY-RUN: Would reboot/restart %s, but dry-run mode is enabled", component)
		return fmt.Errorf("DRY-RUN: Would reboot/restart %s", component) // Return an error to signal dry run
	}

	switch component {
	case "mdb":
		const requiredStandbyDuration = 3 * time.Minute
		const safetyBuffer = 5 * time.Second

		u.logger.Printf("Preparing to reboot MDB. Waiting for vehicle to be in 'stand-by' state for at least %v.", requiredStandbyDuration)

		// Check if we already have a valid standby timestamp
		if !u.standbyStartTime.IsZero() {
			durationInStandby := time.Since(u.standbyStartTime)
			if durationInStandby >= requiredStandbyDuration {
				u.logger.Printf("Vehicle in 'stand-by' for %v (since %s). Proceeding with MDB reboot immediately.", durationInStandby, u.standbyStartTime.Format(time.RFC3339))
				u.logger.Printf("Triggering MDB reboot via Redis command")
				return u.redis.TriggerReboot()
			}

			// Calculate exact remaining time plus safety buffer
			remainingTime := requiredStandbyDuration - durationInStandby + safetyBuffer
			u.logger.Printf("Vehicle in 'stand-by' for %v (since %s). Sleeping for %v then rebooting.", durationInStandby, u.standbyStartTime.Format(time.RFC3339), remainingTime)

			// Sleep for the exact remaining time plus buffer
			select {
			case <-u.ctx.Done():
				return u.ctx.Err()
			case <-time.After(remainingTime):
				// Verify still in standby before rebooting
				currentState, err := u.redis.GetVehicleState(config.VehicleHashKey)
				if err != nil {
					u.logger.Printf("Failed to verify vehicle state before reboot: %v. Proceeding anyway.", err)
				} else if currentState != "stand-by" {
					u.logger.Printf("Vehicle left 'stand-by' state (current: %s) during wait. Restarting wait process.", currentState)
					return u.waitForStandbyWithSubscription(requiredStandbyDuration)
				}

				totalDuration := time.Since(u.standbyStartTime)
				u.logger.Printf("Vehicle has been in 'stand-by' for %v (since %s). Proceeding with MDB reboot.", totalDuration, u.standbyStartTime.Format(time.RFC3339))
				u.logger.Printf("Triggering MDB reboot via Redis command")
				return u.redis.TriggerReboot()
			}
		}

		// No standby timestamp, need to wait for vehicle to enter standby
		u.logger.Printf("Vehicle not in 'stand-by' or no timestamp available. Monitoring for state changes.")
		return u.waitForStandbyWithSubscription(requiredStandbyDuration)

	case "dbc":
		u.logger.Printf("DBC update installed. Will apply on next power cycle.")
		// For DBC, we don't actively reboot - it will apply the update on next power-on
		// Status remains "rebooting" and will be cleared on next service startup
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

// waitForStandbyTimer waits for a specific duration and then checks if still in standby >= 3m
func (u *Updater) waitForStandbyTimer(waitDuration time.Duration) error {
	u.logger.Printf("Starting timer for %v", waitDuration)
	timer := time.NewTimer(waitDuration)
	defer timer.Stop()

	select {
	case <-u.ctx.Done():
		u.logger.Printf("MDB reboot cancelled due to context done.")
		return u.ctx.Err()
	case <-timer.C:
		// Timer expired, check if still in standby and >= 3m total
		if u.standbyStartTime.IsZero() {
			u.logger.Printf("Timer expired but no standby timestamp. Restarting wait.")
			return u.waitForStandbyWithSubscription(3 * time.Minute)
		}

		totalDuration := time.Since(u.standbyStartTime)
		if totalDuration >= 3*time.Minute {
			// Double-check vehicle is still in standby
			currentState, err := u.redis.GetVehicleState(config.VehicleHashKey)
			if err != nil {
				u.logger.Printf("Failed to get vehicle state before reboot: %v. Proceeding anyway.", err)
			} else if currentState != "stand-by" {
				u.logger.Printf("Vehicle left 'stand-by' state (current: %s). Restarting wait.", currentState)
				return u.waitForStandbyWithSubscription(3 * time.Minute)
			}

			u.logger.Printf("Vehicle has been in 'stand-by' for %v (since %s). Proceeding with MDB reboot.", totalDuration, u.standbyStartTime.Format(time.RFC3339))
			u.logger.Printf("Triggering MDB reboot via Redis command")
			return u.redis.TriggerReboot()
		} else {
			// Still haven't reached 3m, wait more
			remainingTime := 3*time.Minute - totalDuration
			u.logger.Printf("Timer expired but only %v elapsed since standby start. Waiting %v more.", totalDuration, remainingTime)
			return u.waitForStandbyTimer(remainingTime)
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
				// Set standby timestamp
				if !stateTimestamp.IsZero() {
					u.standbyStartTime = stateTimestamp
				} else {
					u.standbyStartTime = time.Now()
				}
				u.logger.Printf("Vehicle entered 'stand-by' state at %s. Monitoring for %v.", u.standbyStartTime.Format(time.RFC3339), requiredDuration)

				// Check if already waited long enough
				durationInStandby := time.Since(u.standbyStartTime)
				if durationInStandby >= requiredDuration {
					u.logger.Printf("Vehicle has been in 'stand-by' for %v. Proceeding with MDB reboot.", durationInStandby)
					u.logger.Printf("Triggering MDB reboot via Redis command")
					return u.redis.TriggerReboot()
				}
				// Start precise timer for remaining duration
				remainingTime := requiredDuration - durationInStandby
				u.logger.Printf("Vehicle in 'stand-by' for %v. Starting precise timer for %v.", durationInStandby, remainingTime)
				return u.waitForStandbyTimer(remainingTime)
			} else {
				if !u.standbyStartTime.IsZero() {
					u.logger.Printf("Vehicle left 'stand-by' state (current: %s). Resetting standby timer.", currentState)
					u.standbyStartTime = time.Time{}
				} else {
					u.logger.Printf("Vehicle not in 'stand-by' (current: %s). Waiting.", currentState)
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

// findNextRelease finds the chronologically next release after the current version
func (u *Updater) findNextRelease(releases []Release, currentVersion, channel, variantID string) (Release, bool) {
	var candidateReleases []Release

	// Filter releases for our channel and variant
	for _, release := range releases {
		// Check if the release is for the specified channel
		if !strings.HasPrefix(release.TagName, channel+"-") {
			continue
		}

		// Check if the release has assets for the specified variant
		hasVariantAsset := false
		for _, asset := range release.Assets {
			if strings.Contains(asset.Name, variantID) && (strings.HasSuffix(asset.Name, ".mender") || strings.HasSuffix(asset.Name, ".delta")) {
				hasVariantAsset = true
				break
			}
		}

		if hasVariantAsset {
			candidateReleases = append(candidateReleases, release)
		}
	}

	// Sort releases by tag name (which includes the timestamp)
	// Tags are in format: channel-YYYYMMDDTHHMMSS
	// Use case-insensitive comparison to handle both 't' and 'T' separators
	for i := 0; i < len(candidateReleases)-1; i++ {
		for j := i + 1; j < len(candidateReleases); j++ {
			if strings.ToLower(candidateReleases[i].TagName) > strings.ToLower(candidateReleases[j].TagName) {
				candidateReleases[i], candidateReleases[j] = candidateReleases[j], candidateReleases[i]
			}
		}
	}

	// Find the first release that's newer than the current version
	// Normalize currentTag to lowercase for consistent comparison
	currentTag := strings.ToLower(currentVersion)
	if !strings.HasPrefix(currentTag, channel+"-") {
		currentTag = strings.ToLower(channel + "-" + currentVersion)
	}

	for _, release := range candidateReleases {
		if strings.ToLower(release.TagName) > currentTag {
			u.logger.Printf("Found next release after %s: %s", currentTag, release.TagName)
			return release, true
		}
	}

	u.logger.Printf("No release found after current version %s", currentTag)
	return Release{}, false
}

// findDeltaAsset finds a delta asset in a release for the specified variant
func (u *Updater) findDeltaAsset(release Release, variantID string) string {
	for _, asset := range release.Assets {
		// Match delta assets by variant_id
		// Asset names should be like: librescoot-{variant_id}-{timestamp}.delta
		if strings.Contains(asset.Name, variantID) && strings.HasSuffix(asset.Name, ".delta") {
			u.logger.Printf("Found delta asset: %s", asset.Name)
			return asset.BrowserDownloadURL
		}
	}
	return ""
}

// findMenderAsset finds a mender asset in a release for the specified variant
func (u *Updater) findMenderAsset(release Release, variantID string) string {
	for _, asset := range release.Assets {
		// Match mender assets by variant_id
		if strings.Contains(asset.Name, variantID) && strings.HasSuffix(asset.Name, ".mender") {
			u.logger.Printf("Found mender asset: %s", asset.Name)
			return asset.BrowserDownloadURL
		}
	}
	return ""
}

// buildDeltaChain builds a complete chain of deltas from currentVersion to the latest release
// Returns nil if already at latest version, or error if chain cannot be built
func (u *Updater) buildDeltaChain(releases []Release, currentVersion, channel, variantID string) ([]Release, error) {
	// Filter and sort releases for our channel and variant
	var candidateReleases []Release
	for _, release := range releases {
		// Check if the release is for the specified channel
		if !strings.HasPrefix(release.TagName, channel+"-") {
			continue
		}

		// Check if the release has delta assets for the specified variant
		hasDelta := false
		for _, asset := range release.Assets {
			if strings.Contains(asset.Name, variantID) && strings.HasSuffix(asset.Name, ".delta") {
				hasDelta = true
				break
			}
		}

		if hasDelta {
			candidateReleases = append(candidateReleases, release)
		}
	}

	// Sort releases by tag name (ascending chronological order)
	sort.Slice(candidateReleases, func(i, j int) bool {
		return strings.ToLower(candidateReleases[i].TagName) < strings.ToLower(candidateReleases[j].TagName)
	})

	// Find where we are in the chain
	currentTag := strings.ToLower(currentVersion)
	if !strings.HasPrefix(currentTag, channel+"-") {
		currentTag = strings.ToLower(channel + "-" + currentVersion)
	}
	var deltaChain []Release
	foundCurrent := false

	for _, release := range candidateReleases {
		if strings.ToLower(release.TagName) == currentTag {
			foundCurrent = true
			continue
		}

		// Collect all releases after current version
		if foundCurrent && strings.ToLower(release.TagName) > currentTag {
			deltaChain = append(deltaChain, release)
		}
	}

	// If no deltas needed, return nil (already at latest)
	if len(deltaChain) == 0 {
		return nil, nil
	}

	return deltaChain, nil
}
