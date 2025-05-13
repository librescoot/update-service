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
	"github.com/librescoot/update-service/internal/power"
	"github.com/librescoot/update-service/internal/redis"
	"github.com/librescoot/update-service/internal/vehicle"
)

// Update type constants
const (
	UpdateTypeNone        = "none"
	UpdateTypeAvailable   = "available"
	UpdateTypeDownloading = "downloading"
	UpdateTypeInstalling  = "installing"
	UpdateTypeWaitReboot  = "waiting-reboot"
	UpdateTypeRebooting   = "rebooting"
	UpdateTypeComplete    = "complete"
	UpdateTypeFailed      = "failed"
)

// Updater represents the update orchestrator
type Updater struct {
	config          *config.Config
	redis           *redis.Client
	vehicle         *vehicle.Service
	inhibitor       *inhibitor.Client
	power           *power.Client
	logger          *log.Logger
	ctx             context.Context
	cancel          context.CancelFunc
	githubAPI       *GitHubAPI
	httpServer      *http.Server
	httpServerMutex sync.Mutex
}

// New creates a new updater
func New(ctx context.Context, cfg *config.Config, redisClient *redis.Client, vehicleService *vehicle.Service, inhibitorClient *inhibitor.Client, powerClient *power.Client, logger *log.Logger) *Updater {
	updaterCtx, cancel := context.WithCancel(ctx)
	return &Updater{
		config:          cfg,
		redis:           redisClient,
		vehicle:         vehicleService,
		inhibitor:       inhibitorClient,
		power:           powerClient,
		logger:          logger,
		ctx:             updaterCtx,
		cancel:          cancel,
		githubAPI:       NewGitHubAPI(updaterCtx, cfg.GitHubReleasesURL, logger),
		httpServerMutex: sync.Mutex{},
	}
}

// Start starts the updater
func (u *Updater) Start() error {
	u.logger.Printf("Starting updater with check interval: %v", u.config.CheckInterval)

	// Start the update check loop
	go u.updateCheckLoop()

	return nil
}

// Stop stops the updater
func (u *Updater) Stop() {
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
	u.logger.Printf("Starting update check process")

	// Check current versions
	dbcVersion, err := u.redis.GetComponentVersion("dbc")
	if err != nil {
		u.logger.Printf("Failed to get DBC version: %v", err)
	}
	mdbVersion, err := u.redis.GetComponentVersion("mdb")
	if err != nil {
		u.logger.Printf("Failed to get MDB version: %v", err)
	}

	u.logger.Printf("Current versions - DBC: %s, MDB: %s", dbcVersion, mdbVersion)

	// Get releases from GitHub
	u.logger.Printf("Fetching releases from GitHub")
	releases, err := u.githubAPI.GetReleases()
	if err != nil {
		u.logger.Printf("Failed to get releases: %v", err)
		return
	}

	u.logger.Printf("Successfully fetched %d releases", len(releases))

	// Find latest releases for each component
	var dbcUpdate, mdbUpdate *updateInfo
	var dbcNeedsUpdate, mdbNeedsUpdate bool

	// Check DBC update
	latestDbcRelease, found := u.findLatestRelease(releases, "dbc", u.config.DefaultChannel)
	if found {
		dbcAssetURL := u.findAssetURL(latestDbcRelease, "dbc")
		if dbcAssetURL != "" {
			// For DBC, if version is unset, defer update check
			if dbcVersion == "" {
				u.logger.Printf("DBC version is unset, deferring update check")
			} else {
				// Check if update is needed
				dbcNeedsUpdate = u.isUpdateNeeded("dbc", dbcVersion, latestDbcRelease)
				if dbcNeedsUpdate {
					u.logger.Printf("DBC update available: %s", latestDbcRelease.TagName)
					dbcUpdate = &updateInfo{
						component: "dbc",
						release:   latestDbcRelease,
						assetURL:  dbcAssetURL,
					}
				} else {
					u.logger.Printf("DBC is up to date")
				}
			}
		}
	}

	// Check MDB update
	latestMdbRelease, found := u.findLatestRelease(releases, "mdb", u.config.DefaultChannel)
	if found {
		mdbAssetURL := u.findAssetURL(latestMdbRelease, "mdb")
		if mdbAssetURL != "" {
			// Check if update is needed
			mdbNeedsUpdate = u.isUpdateNeeded("mdb", mdbVersion, latestMdbRelease)
			if mdbNeedsUpdate {
				u.logger.Printf("MDB update available: %s", latestMdbRelease.TagName)
				mdbUpdate = &updateInfo{
					component: "mdb",
					release:   latestMdbRelease,
					assetURL:  mdbAssetURL,
				}
			} else {
				u.logger.Printf("MDB is up to date")
			}
		}
	}

	// If updates are needed, process them
	if dbcNeedsUpdate || mdbNeedsUpdate {
		u.processUpdates(dbcUpdate, mdbUpdate)
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

// findAssetURL finds the asset URL for the given component in the release
func (u *Updater) findAssetURL(release Release, component string) string {
	for _, asset := range release.Assets {
		if strings.Contains(asset.Name, component) && strings.HasSuffix(asset.Name, ".mender") {
			return asset.BrowserDownloadURL
		}
	}
	return ""
}

// isUpdateNeeded checks if an update is needed for the given component
func (u *Updater) isUpdateNeeded(component, currentVersion string, release Release) bool {
	// If no version is installed, an update is needed
	if currentVersion == "" {
		u.logger.Printf("No %s version found, update needed", component)
		return true
	}

	// Extract the timestamp part from the release tag (format: nightly-20250506T214046)
	parts := strings.Split(release.TagName, "-")
	if len(parts) < 2 {
		u.logger.Printf("Invalid release tag format: %s", release.TagName)
		return false
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

// updateInfo represents information about an available update
type updateInfo struct {
	component string
	release   Release
	assetURL  string
}

// processUpdates processes the available updates
func (u *Updater) processUpdates(dbcUpdate, mdbUpdate *updateInfo) {
	u.logger.Printf("Processing updates - DBC: %v, MDB: %v", dbcUpdate != nil, mdbUpdate != nil)

	// Set update status to available
	if err := u.redis.SetOTAStatus(config.OtaStatusHashKey, "update-type", UpdateTypeAvailable); err != nil {
		u.logger.Printf("Failed to set update-type: %v", err)
	}

	// Request ondemand governor for better download performance
	if err := u.power.RequestOndemandGovernor(); err != nil {
		u.logger.Printf("Failed to request ondemand governor: %v", err)
	}

	// Process DBC update if available
	if dbcUpdate != nil {
		// Add download inhibit
		if err := u.inhibitor.AddDownloadInhibit("dbc"); err != nil {
			u.logger.Printf("Failed to add download inhibit for DBC: %v", err)
		}

		// Download DBC update file to MDB filesystem
		localDbcURL, err := u.downloadDbcUpdate(dbcUpdate.assetURL)
		if err != nil {
			u.logger.Printf("Failed to download DBC update: %v", err)
			// Clean up inhibits on failure
			u.inhibitor.RemoveDownloadInhibit("dbc")
			return
		}

		// Start local HTTP server
		if err := u.startHttpServer(); err != nil {
			u.logger.Printf("Failed to start HTTP server: %v", err)
			// Clean up inhibits on failure
			u.inhibitor.RemoveDownloadInhibit("dbc")
			return
		}

		// Add install inhibit before removing download inhibit to prevent power state changes
		if err := u.inhibitor.AddInstallInhibit("dbc"); err != nil {
			u.logger.Printf("Failed to add install inhibit for DBC: %v", err)
		}
		if err := u.inhibitor.RemoveDownloadInhibit("dbc"); err != nil {
			u.logger.Printf("Failed to remove download inhibit for DBC: %v", err)
		}
		if err := u.power.RequestPerformanceGovernor(); err != nil {
			u.logger.Printf("Failed to request performance governor: %v", err)
		}

		// Tell vehicle-service to power on DBC
		if err := u.redis.PushUpdateCommand("start-dbc"); err != nil {
			u.logger.Printf("Failed to send start-dbc command: %v", err)
			// Clean up inhibits on failure
			u.inhibitor.RemoveInstallInhibit("dbc")
			return
		}

		// Wait for DBC readiness
		dbcReady := u.waitForDashboardReady(30 * time.Second)
		if !dbcReady {
			u.logger.Printf("DBC not ready after timeout")
			// Clean up inhibits on failure
			u.inhibitor.RemoveInstallInhibit("dbc")
			return
		}

		// Push update URL to Redis
		if err := u.redis.SetOTAStatus(config.OtaStatusHashKey, "update-component", "dbc"); err != nil {
			u.logger.Printf("Failed to set update-component: %v", err)
		}
		if err := u.redis.SetOTAStatus(config.OtaStatusHashKey, "update-type", UpdateTypeDownloading); err != nil {
			u.logger.Printf("Failed to set update-type: %v", err)
		}
		if err := u.redis.PushUpdateURL(u.config.DbcUpdateKey, localDbcURL); err != nil {
			u.logger.Printf("Failed to push DBC update URL: %v", err)
			// Clean up inhibits on failure
			u.inhibitor.RemoveInstallInhibit("dbc")
			return
		}

		// Wait for DBC update to complete
		u.waitForUpdateCompletion("dbc")

		// Remove install inhibit after update completes
		if err := u.inhibitor.RemoveInstallInhibit("dbc"); err != nil {
			u.logger.Printf("Failed to remove install inhibit for DBC: %v", err)
		}

		// Check if it's safe to power cycle/turn off the DBC
		safe, err := u.vehicle.IsSafeForDbcReboot()
		if err != nil {
			u.logger.Printf("Failed to check if safe for DBC reboot: %v", err)
		} else if safe {
			// Reboot DBC
			if err := u.redis.SetOTAStatus(config.OtaStatusHashKey, "update-type", UpdateTypeRebooting); err != nil {
				u.logger.Printf("Failed to set update-type: %v", err)
			}
			if err := u.vehicle.TriggerReboot("dbc"); err != nil {
				u.logger.Printf("Failed to trigger DBC reboot: %v", err)
			}

			// Wait for dashboard readiness
			dbcReady = u.waitForDashboardReady(60 * time.Second)
			if !dbcReady {
				u.logger.Printf("DBC not ready after reboot")
			} else {
				// Turn DBC off
				if err := u.redis.PushUpdateCommand("complete-dbc"); err != nil {
					u.logger.Printf("Failed to send complete-dbc command: %v", err)
				}
			}
		}
	}

	// Process MDB update if available
	if mdbUpdate != nil {
		// Add download inhibit
		if err := u.inhibitor.AddDownloadInhibit("mdb"); err != nil {
			u.logger.Printf("Failed to add download inhibit for MDB: %v", err)
		}

		// Push update URL to Redis
		if err := u.redis.SetOTAStatus(config.OtaStatusHashKey, "update-component", "mdb"); err != nil {
			u.logger.Printf("Failed to set update-component: %v", err)
		}
		if err := u.redis.SetOTAStatus(config.OtaStatusHashKey, "update-type", UpdateTypeDownloading); err != nil {
			u.logger.Printf("Failed to set update-type: %v", err)
		}
		if err := u.redis.PushUpdateURL(u.config.MdbUpdateKey, mdbUpdate.assetURL); err != nil {
			u.logger.Printf("Failed to push MDB update URL: %v", err)
			// Clean up inhibits on failure
			u.inhibitor.RemoveDownloadInhibit("mdb")
			return
		}

		// Add install inhibit before removing download inhibit to prevent power state changes
		if err := u.inhibitor.AddInstallInhibit("mdb"); err != nil {
			u.logger.Printf("Failed to add install inhibit for MDB: %v", err)
		}
		if err := u.inhibitor.RemoveDownloadInhibit("mdb"); err != nil {
			u.logger.Printf("Failed to remove download inhibit for MDB: %v", err)
		}
		if err := u.power.RequestPerformanceGovernor(); err != nil {
			u.logger.Printf("Failed to request performance governor: %v", err)
		}

		// Wait for MDB update to complete
		u.waitForUpdateCompletion("mdb")

		// Remove install inhibit after update completes
		if err := u.inhibitor.RemoveInstallInhibit("mdb"); err != nil {
			u.logger.Printf("Failed to remove install inhibit for MDB: %v", err)
		}

		// Check if it's safe to reboot MDB
		safe, err := u.vehicle.IsSafeForMdbReboot()
		if err != nil {
			u.logger.Printf("Failed to check if safe for MDB reboot: %v", err)
		} else if safe {
			// Reboot MDB
			if err := u.redis.SetOTAStatus(config.OtaStatusHashKey, "update-type", UpdateTypeRebooting); err != nil {
				u.logger.Printf("Failed to set update-type: %v", err)
			}
			if err := u.vehicle.TriggerReboot("mdb"); err != nil {
				u.logger.Printf("Failed to trigger MDB reboot: %v", err)
			}
		}
	}

	// Set update status to complete
	if err := u.redis.SetOTAStatus(config.OtaStatusHashKey, "update-type", UpdateTypeComplete); err != nil {
		u.logger.Printf("Failed to set update-type: %v", err)
	}
}

// downloadDbcUpdate downloads the DBC update file to the MDB filesystem
func (u *Updater) downloadDbcUpdate(url string) (string, error) {
	// Create the download directory
	downloadDir := "/data/ota"
	if err := os.MkdirAll(downloadDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create download directory: %w", err)
	}

	// Extract filename from URL
	urlParts := strings.Split(url, "/")
	fileName := urlParts[len(urlParts)-1]
	if fileName == "" {
		fileName = "dbc-update.mender"
	}

	// Download the file
	filePath := filepath.Join(downloadDir, fileName)
	u.logger.Printf("Downloading DBC update to: %s", filePath)

	// Create the file
	out, err := os.Create(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to create file: %w", err)
	}
	defer out.Close()

	// Get the data
	resp, err := http.Get(url)
	if err != nil {
		return "", fmt.Errorf("failed to GET from URL: %w", err)
	}
	defer resp.Body.Close()

	// Check server response
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("bad status: %s", resp.Status)
	}

	// Write the body to file
	_, err = io.Copy(out, resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to copy content: %w", err)
	}

	// Return the local URL
	return fmt.Sprintf("http://192.168.7.1:8000/ota/%s", fileName), nil
}

// startHttpServer starts a local HTTP server to serve the DBC update file
func (u *Updater) startHttpServer() error {
	u.httpServerMutex.Lock()
	defer u.httpServerMutex.Unlock()

	// If server is already running, return
	if u.httpServer != nil {
		return nil
	}

	// Create a new server
	mux := http.NewServeMux()
	mux.HandleFunc("/ota/", func(w http.ResponseWriter, r *http.Request) {
		// Extract the filename from the URL
		fileName := filepath.Base(r.URL.Path)
		filePath := filepath.Join("/data/ota", fileName)

		// Check if file exists
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			http.Error(w, "File not found", http.StatusNotFound)
			return
		}

		// Set headers
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", fileName))
		w.Header().Set("Content-Type", "application/octet-stream")

		// Serve the file
		http.ServeFile(w, r, filePath)
	})

	// Create server with reasonable timeouts
	u.httpServer = &http.Server{
		Addr:         ":8000",
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	// Start the server
	go func() {
		u.logger.Printf("Starting HTTP server on port 8000")
		if err := u.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			u.logger.Printf("HTTP server error: %v", err)
		}
	}()

	return nil
}

// waitForDashboardReady waits for the dashboard to be ready
func (u *Updater) waitForDashboardReady(timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(u.ctx, timeout)
	defer cancel()

	readyChan := u.redis.SubscribeToDashboardReady(ctx, "dashboard")
	if readyChan == nil {
		u.logger.Printf("Failed to subscribe to dashboard ready channel")
		return false
	}

	select {
	case <-readyChan:
		u.logger.Printf("Dashboard ready signal received")
		return true
	case <-ctx.Done():
		u.logger.Printf("Timed out waiting for dashboard ready")
		return false
	}
}

// waitForUpdateCompletion waits for an update to complete
func (u *Updater) waitForUpdateCompletion(component string) {
	// Subscribe to OTA status changes
	otaMessages, cleanup, err := u.redis.SubscribeToOTAStatus(config.OtaChannel)
	if err != nil {
		u.logger.Printf("Failed to subscribe to OTA status: %v", err)
		return
	}
	defer cleanup()

	// Wait for update to complete or timeout
	timeout := time.After(30 * time.Minute)
	for {
		select {
		case <-timeout:
			u.logger.Printf("Timed out waiting for %s update to complete", component)
			return
		case msg := <-otaMessages:
			// Check if message indicates update completion
			if msg == "status" {
				status, err := u.redis.GetOTAStatus(config.OtaStatusHashKey)
				if err != nil {
					u.logger.Printf("Failed to get OTA status: %v", err)
					continue
				}

				updateType := status["update-type"]
				updateComponent := status["update-component"]

				if updateComponent == component && (updateType == UpdateTypeComplete || updateType == UpdateTypeFailed || updateType == UpdateTypeWaitReboot) {
					u.logger.Printf("%s update completed with status: %s", component, updateType)
					return
				}
			}
		case <-u.ctx.Done():
			u.logger.Printf("Context cancelled while waiting for %s update", component)
			return
		}
	}
}
