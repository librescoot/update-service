package updater

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
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
	config           *config.Config
	redis            *redis.Client
	vehicle          *vehicle.Service
	inhibitor        *inhibitor.Client
	logger           *log.Logger
	ctx              context.Context
	cancel           context.CancelFunc
	githubAPI        *GitHubAPI
	updateState      map[string]string
	stateMutex       sync.Mutex
	otaMessages      <-chan string
	cleanupSub       func()
	httpServer       *http.Server
	httpServerMutex  sync.Mutex
	dbcUpdateFile    string
	dbcDownloadReady chan struct{}
}

// New creates a new updater
func New(ctx context.Context, cfg *config.Config, redisClient *redis.Client, vehicleService *vehicle.Service, inhibitorClient *inhibitor.Client, logger *log.Logger) *Updater {
	updaterCtx, cancel := context.WithCancel(ctx)
	return &Updater{
		config:           cfg,
		redis:            redisClient,
		vehicle:          vehicleService,
		inhibitor:        inhibitorClient,
		logger:           logger,
		ctx:              updaterCtx,
		cancel:           cancel,
		githubAPI:        NewGitHubAPI(updaterCtx, cfg.GitHubReleasesURL, logger),
		updateState:      make(map[string]string),
		stateMutex:       sync.Mutex{},
		httpServerMutex:  sync.Mutex{},
		dbcDownloadReady: make(chan struct{}),
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
	currentStatus := status["status"]
	
	// Update our internal state map first
	u.stateMutex.Lock()
	prevState := u.updateState[component]
	u.updateState[component] = currentStatus
	u.stateMutex.Unlock()
	
	u.logger.Printf("Component %s state changed: %s -> %s", component, prevState, currentStatus)

	// Variable to track if waiting for reboot
	isWaitingReboot := currentStatus == "installation-complete-waiting-reboot"

	switch currentStatus {
	case "downloading":
		u.logger.Printf("Update downloading for %s", component)

		// MDB updates should set a delay inhibit (can be interrupted if needed)
		// DBC updates should also set a delay inhibit since they're less critical
		if err := u.inhibitor.AddDownloadInhibit(component); err != nil {
			u.logger.Printf("Failed to add download inhibit for %s: %v", component, err)
		}

	case "installing":
		u.logger.Printf("Update installing for %s", component)

		// Remove download inhibit if it exists
		if err := u.inhibitor.RemoveDownloadInhibit(component); err != nil {
			u.logger.Printf("Failed to remove download inhibit for %s: %v", component, err)
		}

		// MDB updates that are already installing should block power state changes
		// DBC updates should also block once installing since interrupting would be problematic
		if err := u.inhibitor.AddInstallInhibit(component); err != nil {
			u.logger.Printf("Failed to add install inhibit for %s: %v", component, err)
		}

		// If this is the DBC starting installation, make sure dashboard power stays on
		if component == "dbc" {
			if err := u.redis.PushUpdateCommand("start-dbc"); err != nil {
				u.logger.Printf("Failed to send start-dbc command: %v", err)
			}
		}

	case "installation-complete-waiting-reboot":
		u.logger.Printf("Update installed for %s, waiting for reboot", component)
		
		// Keep inhibitors active until after reboot for DBC
		if component == "dbc" {
			// For DBC, we keep the install inhibit active until after reboot
			// to ensure the DBC doesn't get powered off prematurely
			u.logger.Printf("Keeping install inhibit active for DBC until rebooted")
			
			// Notify vehicle service that DBC update is complete
			if err := u.redis.PushUpdateCommand("complete-dbc"); err != nil {
				u.logger.Printf("Failed to send complete-dbc command: %v", err)
			}

			// Trigger DBC reboot to apply update
			u.logger.Printf("Reboot needed for DBC")
			if err := u.vehicle.TriggerReboot("dbc"); err != nil {
				u.logger.Printf("Failed to trigger DBC reboot: %v", err)
			}
		} else if component == "mdb" {
			// For MDB, we can remove the inhibits now
			u.removeUpdateInhibits(component)
			
			u.logger.Printf("Update installed for MDB, waiting for reboot")
			
			// Notify vehicle service that MDB update is complete
			// This allows the vehicle service to set the appropriate power state
			if err := u.redis.PushUpdateCommand("complete"); err != nil {
				u.logger.Printf("Failed to send complete command: %v", err)
			}
			
			// Wait a moment for the vehicle service to process the command and update state
			time.Sleep(2 * time.Second)
			
			u.logger.Printf("Reboot needed for MDB")
			
			// Now check if it's safe to reboot
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

	case "complete":
		u.logger.Printf("Update complete for %s", component)
		
		// Remove all inhibits
		u.removeUpdateInhibits(component)
		
		// For DBC, notify vehicle service that update is complete
		if component == "dbc" {
			if err := u.redis.PushUpdateCommand("complete-dbc"); err != nil {
				u.logger.Printf("Failed to send complete-dbc command: %v", err)
			}
		}

	case "failed":
		u.logger.Printf("Update failed for %s: %s", component, status["error"])
		
		// Remove all inhibits on failure
		u.removeUpdateInhibits(component)
		
		// For DBC, notify vehicle service that update is complete (failed)
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

	// Stop the HTTP server if running
	u.stopHttpServer()

	u.cancel()
}

// stopHttpServer stops the HTTP server if it's running
func (u *Updater) stopHttpServer() {
	u.httpServerMutex.Lock()
	defer u.httpServerMutex.Unlock()

	if u.httpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		u.logger.Printf("Shutting down HTTP server")
		if err := u.httpServer.Shutdown(ctx); err != nil {
			u.logger.Printf("HTTP server shutdown error: %v", err)
		}
		u.httpServer = nil
	}

	// Clean up the downloaded file if it exists
	if u.dbcUpdateFile != "" {
		u.logger.Printf("Cleaning up downloaded file: %s", u.dbcUpdateFile)
		if err := os.Remove(u.dbcUpdateFile); err != nil {
			u.logger.Printf("Error removing downloaded file: %v", err)
		}
		u.dbcUpdateFile = ""
	}
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

	// Sequence updates: Apply both MDB and DBC updates sequentially
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

		// Queue both updates to be processed sequentially
		if mdbUpdate != nil || dbcUpdate != nil {
			go u.processUpdatesSequentially(mdbUpdate, dbcUpdate)
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

// removeUpdateInhibits removes all inhibits for a component
func (u *Updater) removeUpdateInhibits(component string) {
	// Remove download inhibit if it exists
	if err := u.inhibitor.RemoveDownloadInhibit(component); err != nil {
		// Log but don't fail if the inhibit doesn't exist
		if !strings.Contains(err.Error(), "does not exist") {
			u.logger.Printf("Failed to remove download inhibit for %s: %v", component, err)
		}
	}

	// Remove install inhibit if it exists
	if err := u.inhibitor.RemoveInstallInhibit(component); err != nil {
		// Log but don't fail if the inhibit doesn't exist
		if !strings.Contains(err.Error(), "does not exist") {
			u.logger.Printf("Failed to remove install inhibit for %s: %v", component, err)
		}
	}
}

// processUpdatesSequentially handles the sequential processing of MDB and DBC updates
func (u *Updater) processUpdatesSequentially(mdbUpdate, dbcUpdate *updateInfo) {
	// Track if we have active updates
	u.stateMutex.Lock()
	activeUpdates := u.hasUpdatesInProgress()
	u.stateMutex.Unlock()

	if activeUpdates {
		u.logger.Printf("Updates already in progress, skipping this update cycle")
		return
	}

	// Create a local HTTP server to serve DBC updates if both updates are present
	dbcLocalURL := ""
	if mdbUpdate != nil && dbcUpdate != nil {
		var err error
		dbcLocalURL, err = u.setupLocalUpdateServer(dbcUpdate.assetURL)
		if err != nil {
			u.logger.Printf("Failed to set up local update server for DBC: %v", err)
			// Continue with the original URL if local server setup fails
			dbcLocalURL = ""
		}
	}

	// Apply MDB update first if available
	if mdbUpdate != nil {
		u.logger.Printf("Initiating MDB update")
		if err := u.initiateUpdate(mdbUpdate.component, mdbUpdate.release); err != nil {
			u.logger.Printf("Failed to initiate MDB update: %v", err)
			return // Stop processing if MDB update fails
		}

		// Wait for MDB update to complete before proceeding to DBC
		complete := u.waitForComponentUpdate("mdb", 30*time.Minute)
		if !complete {
			u.logger.Printf("MDB update timed out or failed, skipping DBC update")
			return
		}
		u.logger.Printf("MDB update completed successfully")
	}

	// Now apply DBC update if available
	if dbcUpdate != nil {
		// Ensure dashboard power is on before starting DBC update
		if err := u.redis.PushUpdateCommand("start-dbc"); err != nil {
			u.logger.Printf("Failed to send start-dbc command: %v", err)
			return
		}

		// Wait for dashboard to be ready (subscribe to dashboard ready signal)
		u.logger.Printf("Waiting for dashboard to be ready")
		dashboardReady := u.waitForDashboardReady(30 * time.Second)
		if !dashboardReady {
			u.logger.Printf("Dashboard not ready after timeout, proceeding with update anyway")
		} else {
			u.logger.Printf("Dashboard is ready, proceeding with DBC update")
		}

		// Use local URL if available, otherwise use original URL
		dbcUpdate.release.Tag = "local" // Mark the release as local for proper initiation
		urlToUse := dbcUpdate.assetURL
		if dbcLocalURL != "" {
			urlToUse = dbcLocalURL
			u.logger.Printf("Using local URL for DBC update: %s", dbcLocalURL)
		}

		// Initiate the DBC update
		time.Sleep(3 * time.Second) // Wait a bit to ensure dashboard is fully ready
		dbcUpdate.assetURL = urlToUse // Update the URL to use
		u.logger.Printf("Initiating DBC update")
		if err := u.initiateUpdate(dbcUpdate.component, dbcUpdate.release); err != nil {
			u.logger.Printf("Failed to initiate DBC update: %v", err)
			return
		}

		// Wait for DBC update to complete
		complete := u.waitForComponentUpdate("dbc", 30*time.Minute)
		if !complete {
			u.logger.Printf("DBC update timed out or failed")
		} else {
			u.logger.Printf("DBC update completed successfully")
			
			// Reboot DBC to apply the update
			if err := u.vehicle.TriggerReboot("dbc"); err != nil {
				u.logger.Printf("Failed to trigger DBC reboot: %v", err)
			} else {
				u.logger.Printf("DBC reboot triggered successfully")
			}
		}
	}
}

// waitForComponentUpdate waits for a component update to complete within the given timeout
func (u *Updater) waitForComponentUpdate(component string, timeout time.Duration) bool {
	startTime := time.Now()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// Check if the component update has completed
			u.stateMutex.Lock()
			status, exists := u.updateState[component]
			u.stateMutex.Unlock()

			if exists && (status == "complete" || status == "failed") {
				return status == "complete"
			}

			// Check if we've timed out
			if time.Since(startTime) > timeout {
				u.logger.Printf("%s update timed out after %v", component, timeout)
				return false
			}
		case <-u.ctx.Done():
			return false
		}
	}
}

// waitForDashboardReady waits for the dashboard to be ready
func (u *Updater) waitForDashboardReady(timeout time.Duration) bool {
	// Subscribe to the dashboard ready channel
	ctx, cancel := context.WithTimeout(u.ctx, timeout)
	defer cancel()

	dashboardReadyChan := u.redis.SubscribeToDashboardReady(ctx, "dashboard")
	if dashboardReadyChan == nil {
		u.logger.Printf("Failed to subscribe to dashboard ready channel")
		return false
	}

	select {
	case <-dashboardReadyChan:
		u.logger.Printf("Dashboard ready signal received")
		return true
	case <-ctx.Done():
		u.logger.Printf("Timed out waiting for dashboard ready")
		return false
	}
}

// setupLocalUpdateServer sets up a local HTTP server to serve the DBC update file
// Returns the local URL where the file can be accessed by the DBC
func (u *Updater) setupLocalUpdateServer(remoteURL string) (string, error) {
	// First stop any existing server
	u.stopHttpServer()

	// Create a temporary directory for downloaded files
	tempDir, err := os.MkdirTemp("", "update-service")
	if err != nil {
		return "", fmt.Errorf("failed to create temp directory: %w", err)
	}

	// Extract filename from the remote URL
	urlParts := strings.Split(remoteURL, "/")
	fileName := urlParts[len(urlParts)-1]
	if fileName == "" {
		fileName = "dbc-update.mender"
	}

	// Download the DBC update file
	filePath := filepath.Join(tempDir, fileName)
	u.logger.Printf("Downloading DBC update to: %s", filePath)
	
	go func() {
		if err := u.downloadFile(remoteURL, filePath); err != nil {
			u.logger.Printf("Failed to download DBC update: %v", err)
			return
		}
		u.dbcUpdateFile = filePath
		
		// Signal that the download is ready
		close(u.dbcDownloadReady)
	}()

	// Wait for download to complete or timeout
	select {
	case <-u.dbcDownloadReady:
		u.logger.Printf("DBC update file downloaded successfully")
	case <-time.After(5 * time.Minute):
		u.logger.Printf("Timed out waiting for DBC update download")
		return "", fmt.Errorf("download timeout exceeded")
	}

	// Set up the HTTP server to serve the downloaded file
	mux := http.NewServeMux()
	mux.HandleFunc("/ota/", func(w http.ResponseWriter, r *http.Request) {
		u.logger.Printf("Serving DBC update file: %s", filePath)
		
		// Set appropriate headers
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", fileName))
		w.Header().Set("Content-Type", "application/octet-stream")
		
		// Serve the file
		http.ServeFile(w, r, filePath)
	})

	// Start the HTTP server
	u.httpServerMutex.Lock()
	defer u.httpServerMutex.Unlock()

	u.httpServer = &http.Server{
		Addr:    ":8000",
		Handler: mux,
	}

	go func() {
		u.logger.Printf("Starting HTTP server on port 8000")
		if err := u.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			u.logger.Printf("HTTP server error: %v", err)
		}
	}()

	// Return the local URL
	return fmt.Sprintf("http://192.168.7.1:8000/ota/%s", fileName), nil
}

// downloadFile downloads a file from the given URL to the specified path
func (u *Updater) downloadFile(url, filePath string) error {
	// Create the file
	out, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}
	defer out.Close()

	// Get the data
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("failed to GET from URL: %w", err)
	}
	defer resp.Body.Close()

	// Check server response
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bad status: %s", resp.Status)
	}

	// Writer the body to file
	_, err = io.Copy(out, resp.Body)
	if err != nil {
		return fmt.Errorf("failed to copy content: %w", err)
	}

	return nil
}
