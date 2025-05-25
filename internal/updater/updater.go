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

// updateInfo represents information about an available update
type updateInfo struct {
	component string
	release   Release
	assetURL  string
}

// Updater represents the update orchestrator
type Updater struct {
	config           *config.Config
	redis            *redis.Client
	vehicle          *vehicle.Service
	inhibitor        *inhibitor.Client
	power            *power.Client
	logger           *log.Logger
	ctx              context.Context
	cancel           context.CancelFunc
	githubAPI        *GitHubAPI
	otaMessages      <-chan string
	cleanupSub       func()
	httpServer       *http.Server
	httpServerMutex  sync.Mutex
	dbcUpdateFile    string
	dbcDownloadReady chan struct{}
}

// New creates a new updater
func New(ctx context.Context, cfg *config.Config, redisClient *redis.Client, vehicleService *vehicle.Service, inhibitorClient *inhibitor.Client, powerClient *power.Client, logger *log.Logger) *Updater {
	updaterCtx, cancel := context.WithCancel(ctx)
	return &Updater{
		config:           cfg,
		redis:            redisClient,
		vehicle:          vehicleService,
		inhibitor:        inhibitorClient,
		power:            powerClient,
		logger:           logger,
		ctx:              updaterCtx,
		cancel:           cancel,
		githubAPI:        NewGitHubAPI(updaterCtx, cfg.GitHubReleasesURL, logger),
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
				// Process update-type changes
				status, err := u.redis.GetOTAStatus(config.OtaStatusHashKey)
				if err != nil {
					u.logger.Printf("Failed to get OTA status for update-type change: %v", err)
					continue
				}

				updateType, exists := status["update-type"]
				if !exists {
					u.logger.Printf("update-type field not found in OTA status")
					continue
				}

				u.logger.Printf("Processing update-type change: %s", updateType)
				u.processUpdateTypeChange(updateType, status)

			default:
				u.logger.Printf("Unexpected OTA message: %s", msg)
			}
		}
	}
}

// processUpdateTypeChange handles changes to the update-type field
func (u *Updater) processUpdateTypeChange(updateType string, status map[string]string) {
	// Check component this update applies to
	component, exists := status["update-component"]
	if !exists {
		u.logger.Printf("update-component field not found, cannot process update-type change")
		return
	}

	u.logger.Printf("Processing update-type change for component %s: %s", component, updateType)

	// Handle state transitions based on update-type
	switch updateType {
	case UpdateTypeDownloading:
		// Add download inhibit
		if err := u.inhibitor.AddDownloadInhibit(component); err != nil {
			u.logger.Printf("Failed to add download inhibit for %s: %v", component, err)
		}
		
		// Request ondemand governor for better performance during downloads
		if err := u.power.RequestOndemandGovernor(); err != nil {
			u.logger.Printf("Failed to request ondemand governor for download: %v", err)
		} else {
			u.logger.Printf("Set CPU governor to ondemand for download performance")
		}

	case UpdateTypeInstalling:
		// Remove download inhibit, add install inhibit
		if err := u.inhibitor.RemoveDownloadInhibit(component); err != nil {
			u.logger.Printf("Failed to remove download inhibit for %s: %v", component, err)
		}
		
		// Request performance governor for installation to ensure consistent timing
		if err := u.power.RequestPerformanceGovernor(); err != nil {
			u.logger.Printf("Failed to request performance governor for installation: %v", err)
		} else {
			u.logger.Printf("Set CPU governor to performance for stable installation performance")
		}

		if err := u.inhibitor.AddInstallInhibit(component); err != nil {
			u.logger.Printf("Failed to add install inhibit for %s: %v", component, err)
		}

		// If this is DBC starting installation, make sure dashboard power stays on
		if component == "dbc" {
			if err := u.redis.PushUpdateCommand("start-dbc"); err != nil {
				u.logger.Printf("Failed to send start-dbc command: %v", err)
			}
		}

	case UpdateTypeWaitReboot:
		u.logger.Printf("Update installed for %s, waiting for reboot", component)

		if component == "dbc" {
			u.logger.Printf("DBC update installed, powering down when safe")

			// Remove install inhibit immediately - DBC update is complete
			if err := u.inhibitor.RemoveInstallInhibit(component); err != nil {
				if !strings.Contains(err.Error(), "does not exist") {
					u.logger.Printf("Failed to remove install inhibit for %s: %v", component, err)
				}
			}

			// Check current vehicle state
			vehicleState, err := u.redis.GetVehicleState(config.VehicleHashKey)
			if err != nil {
				u.logger.Printf("Failed to get vehicle state: %v", err)
			} else {
				u.logger.Printf("Current vehicle state during DBC update completion: %s", vehicleState)

				// If the vehicle is in stand-by state, show shutdown message
				if vehicleState == "stand-by" || vehicleState == "updating" {
					if err := u.redis.SetOTAStatus(config.OtaStatusHashKey, "update-will-shutdown", "true"); err != nil {
						u.logger.Printf("Failed to set update shutdown flag: %v", err)
					}
				}
			}

			// Power down DBC when safe (no reboot needed)
			safe, err := u.vehicle.IsSafeForDbcPowerDown()
			if err != nil {
				u.logger.Printf("Failed to check if safe for DBC power down: %v", err)
				return
			}

			if safe {
				u.logger.Printf("Safe to power down DBC after update installation")
				if err := u.redis.PushUpdateCommand("complete-dbc"); err != nil {
					u.logger.Printf("Failed to send complete-dbc command: %v", err)
				}
				
				// Mark DBC update as complete
				if err := u.redis.SetOTAStatus(config.OtaStatusHashKey, "status:dbc", "complete"); err != nil {
					u.logger.Printf("Failed to set DBC status to complete: %v", err)
				}
			} else {
				u.logger.Printf("Not safe to power down DBC now, will power down when vehicle enters standby")
				// DBC will be powered down automatically when vehicle transitions to standby
			}

			// Clear the shutdown flag after a delay (DBC should be powered down by then)
			go func() {
				time.Sleep(10 * time.Second)
				if err := u.redis.SetOTAStatus(config.OtaStatusHashKey, "update-will-shutdown", "false"); err != nil {
					u.logger.Printf("Failed to clear update shutdown flag: %v", err)
				}
			}()
		} else if component == "mdb" {
			// For MDB, we can remove the inhibits now
			u.removeUpdateInhibits(component)

			u.logger.Printf("Update installed for MDB, waiting for reboot")

			// Notify vehicle service that MDB update is complete
			if err := u.redis.PushUpdateCommand("complete"); err != nil {
				u.logger.Printf("Failed to send complete command: %v", err)
			}

			// Wait a moment for the vehicle service to process the command and update state
			time.Sleep(2 * time.Second)

			u.logger.Printf("Reboot needed for MDB")

			// Check if DBC update is complete
			dbcStatus, err := u.redis.GetOTAStatus(config.OtaStatusHashKey)
			if err != nil {
				u.logger.Printf("Failed to get OTA status: %v", err)
			} else {
				dbcStatusValue := dbcStatus["status:dbc"]
				if dbcStatusValue != "" && dbcStatusValue != "complete" && dbcStatusValue != "failed" {
					u.logger.Printf("DBC update still in progress (%s), waiting for completion before MDB reboot", dbcStatusValue)
					u.vehicle.ScheduleMdbRebootCheck(u.config.MdbRebootCheckInterval)
					go u.waitForMdbReboot()
					return
				}
			}

			// Now check if it's safe to reboot
			safe, err := u.vehicle.IsSafeForMdbReboot()
			if err != nil {
				u.logger.Printf("Failed to check if safe for MDB reboot: %v", err)
				return
			}

			if safe {
				// Update state to rebooting
				if err := u.redis.SetOTAStatus(config.OtaStatusHashKey, "update-type", UpdateTypeRebooting); err != nil {
					u.logger.Printf("Failed to set update-type to rebooting: %v", err)
				}
				if err := u.vehicle.TriggerReboot("mdb"); err != nil {
					u.logger.Printf("Failed to trigger MDB reboot: %v", err)
				}
			} else {
				u.logger.Printf("Not safe to reboot MDB, scheduling reboot check")
				u.vehicle.ScheduleMdbRebootCheck(u.config.MdbRebootCheckInterval)
				go u.waitForMdbReboot()
			}
		}

	case UpdateTypeComplete:
		u.logger.Printf("Update complete for %s", component)

		// Remove all inhibits
		u.removeUpdateInhibits(component)
		
		// Let PM service decide best governor based on current system state
		// We don't need to explicitly request powersave here, as PM service
		// will set the appropriate governor based on system state

		// For DBC, notify vehicle service that update is complete
		if component == "dbc" {
			if err := u.redis.PushUpdateCommand("complete-dbc"); err != nil {
				u.logger.Printf("Failed to send complete-dbc command: %v", err)
			}

			// Clear the shutdown flag
			if err := u.redis.SetOTAStatus(config.OtaStatusHashKey, "update-will-shutdown", "false"); err != nil {
				u.logger.Printf("Failed to clear update shutdown flag: %v", err)
			}
		}

	case UpdateTypeFailed:
		u.logger.Printf("Update failed for %s: %s", component, status["error"])

		// Remove all inhibits on failure
		u.removeUpdateInhibits(component)

		// For DBC, notify vehicle service that update is complete (failed)
		if component == "dbc" {
			if err := u.redis.PushUpdateCommand("complete-dbc"); err != nil {
				u.logger.Printf("Failed to send complete-dbc command: %v", err)
			}

			// Clear the shutdown flag
			if err := u.redis.SetOTAStatus(config.OtaStatusHashKey, "update-will-shutdown", "false"); err != nil {
				u.logger.Printf("Failed to clear update shutdown flag: %v", err)
			}
		}
	}
}

// processOTAStatus processes an OTA status update
func (u *Updater) processOTAStatus(status map[string]string) {
	// Process status for all known components
	components := []string{"dbc", "mdb"}
	
	for _, component := range components {
		// Only process if the component has a status
		componentStatusKey := "status:" + component
		if _, exists := status[componentStatusKey]; exists {
			u.processComponentStatus(component, status)
		}
	}
}

// processComponentStatus processes an OTA status update for a specific component
func (u *Updater) processComponentStatus(component string, status map[string]string) {
	u.logger.Printf("Processing status update for component: %s", component)

	// Process component status update

	// Check component-specific status
	componentStatusKey := "status:" + component
	currentStatus := status[componentStatusKey]

	// If we don't have a component-specific status, check the general update-type
	// but only if update-component matches our component
	if currentStatus == "" {
		if status["update-component"] == component {
			currentStatus = status["update-type"]
		}
	}

	// If we still don't have a status, there's nothing to process
	if currentStatus == "" {
		u.logger.Printf("No status found for component %s", component)
		return
	}

	// Get previous state from Redis for logging
	status, err := u.redis.GetOTAStatus(config.OtaStatusHashKey)
	if err != nil {
		u.logger.Printf("Failed to get previous state for %s: %v", component, err)
	}
	prevState := status["status:"+component]

	u.logger.Printf("Component %s state changed: %s -> %s", component, prevState, currentStatus)

	// Update the OTA hash to indicate component update status and component
	if err := u.redis.SetOTAStatus(config.OtaStatusHashKey, "status:"+component, currentStatus); err != nil {
		u.logger.Printf("Failed to update OTA status for %s: %v", component, err)
	}

	// Update the update-type and update-component fields if this component is being updated
	// This maintains compatibility with the new standard fields
	if currentStatus == "downloading" || currentStatus == "installing" ||
		currentStatus == "installation-complete-waiting-reboot" || currentStatus == "rebooting" {
		// Map the legacy status to the new update-type value
		var updateType string
		switch currentStatus {
		case "downloading":
			updateType = UpdateTypeDownloading
		case "installing":
			updateType = UpdateTypeInstalling
		case "installation-complete-waiting-reboot":
			updateType = UpdateTypeWaitReboot
		case "rebooting":
			updateType = UpdateTypeRebooting
		}

		if err := u.redis.SetOTAStatus(config.OtaStatusHashKey, "update-type", updateType); err != nil {
			u.logger.Printf("Failed to set update-type: %v", err)
		}

		if err := u.redis.SetOTAStatus(config.OtaStatusHashKey, "update-component", component); err != nil {
			u.logger.Printf("Failed to set update-component: %v", err)
		}
	} else if currentStatus == "complete" || currentStatus == "failed" {
		// If this component was the active update, clear the update-type
		if status["update-component"] == component {
			var updateType string
			if currentStatus == "complete" {
				updateType = UpdateTypeComplete
			} else {
				updateType = UpdateTypeFailed
			}

			if err := u.redis.SetOTAStatus(config.OtaStatusHashKey, "update-type", updateType); err != nil {
				u.logger.Printf("Failed to set update-type: %v", err)
			}
		}
	}

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

		if component == "dbc" {
			u.logger.Printf("DBC update installed, powering down when safe")

			// Remove install inhibit immediately - DBC update is complete
			if err := u.inhibitor.RemoveInstallInhibit(component); err != nil {
				if !strings.Contains(err.Error(), "does not exist") {
					u.logger.Printf("Failed to remove install inhibit for %s: %v", component, err)
				}
			}

			// Check current vehicle state
			vehicleState, err := u.redis.GetVehicleState(config.VehicleHashKey)
			if err != nil {
				u.logger.Printf("Failed to get vehicle state: %v", err)
			} else {
				u.logger.Printf("Current vehicle state during DBC update completion: %s", vehicleState)

				// If the vehicle is in stand-by state, show shutdown message
				if vehicleState == "stand-by" || vehicleState == "updating" {
					if err := u.redis.SetOTAStatus(config.OtaStatusHashKey, "update-will-shutdown", "true"); err != nil {
						u.logger.Printf("Failed to set update shutdown flag: %v", err)
					}
				}
			}

			// Power down DBC when safe (no reboot needed)
			safe, err := u.vehicle.IsSafeForDbcPowerDown()
			if err != nil {
				u.logger.Printf("Failed to check if safe for DBC power down: %v", err)
				return
			}

			if safe {
				u.logger.Printf("Safe to power down DBC after update installation")
				if err := u.redis.PushUpdateCommand("complete-dbc"); err != nil {
					u.logger.Printf("Failed to send complete-dbc command: %v", err)
				}
				
				// Mark DBC update as complete
				if err := u.redis.SetOTAStatus(config.OtaStatusHashKey, "status:dbc", "complete"); err != nil {
					u.logger.Printf("Failed to set DBC status to complete: %v", err)
				}
			} else {
				u.logger.Printf("Not safe to power down DBC now, will power down when vehicle enters standby")
				// DBC will be powered down automatically when vehicle transitions to standby
			}

			// Clear the shutdown flag after a delay (DBC should be powered down by then)
			go func() {
				time.Sleep(10 * time.Second)
				if err := u.redis.SetOTAStatus(config.OtaStatusHashKey, "update-will-shutdown", "false"); err != nil {
					u.logger.Printf("Failed to clear update shutdown flag: %v", err)
				}
			}()
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
				// Update state to rebooting
				if err := u.redis.SetOTAStatus(config.OtaStatusHashKey, "update-type", UpdateTypeRebooting); err != nil {
					u.logger.Printf("Failed to set update-type to rebooting: %v", err)
				}

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

			// Clear the shutdown flag
			if err := u.redis.SetOTAStatus(config.OtaStatusHashKey, "update-will-shutdown", "false"); err != nil {
				u.logger.Printf("Failed to clear update shutdown flag: %v", err)
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

			// Clear the shutdown flag
			if err := u.redis.SetOTAStatus(config.OtaStatusHashKey, "update-will-shutdown", "false"); err != nil {
				u.logger.Printf("Failed to clear update shutdown flag: %v", err)
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
	// Check Redis for active component updates
	status, err := u.redis.GetOTAStatus(config.OtaStatusHashKey)
	if err != nil {
		u.logger.Printf("Failed to get OTA status: %v", err)
		return false
	}

	// Check if any component is actively updating
	dbcStatus := status["status:dbc"]
	mdbStatus := status["status:mdb"]
	
	activeStates := []string{"downloading", "installing", "waiting-reboot", "rebooting"}
	
	for _, activeState := range activeStates {
		if dbcStatus == activeState {
			u.logger.Printf("DBC is currently updating (%s)", dbcStatus)
			return true
		}
		if mdbStatus == activeState {
			u.logger.Printf("MDB is currently updating (%s)", mdbStatus)
			return true
		}
	}
	
	u.logger.Printf("No updates in progress (dbc:%s, mdb:%s)", dbcStatus, mdbStatus)
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
	u.logger.Printf("Starting update check process")

	// Get releases from GitHub with retries
	u.logger.Printf("Fetching releases from GitHub")
	releases, err := u.githubAPI.GetReleases()
	if err != nil {
		u.logger.Printf("ERROR: Failed to get releases: %v", err)
		return
	}

	u.logger.Printf("Successfully fetched %d releases", len(releases))

	// Find available updates for each component
	var updates []updateInfo
	u.logger.Printf("Checking for updates across %d components", len(u.config.Components))

	for _, component := range u.config.Components {
		u.logger.Printf("Checking component: %s", component)

		// Get the latest release for the component and channel
		u.logger.Printf("Finding latest release for component %s on channel %s",
			component, u.config.DefaultChannel)
		release, found := u.findLatestRelease(releases, component, u.config.DefaultChannel)
		if !found {
			u.logger.Printf("No release found for component %s and channel %s",
				component, u.config.DefaultChannel)
			continue
		}

		// Find the .mender asset for the component
		var menderAsset string
		var assetURL string
		u.logger.Printf("Looking for .mender asset for component %s", component)
		for _, asset := range release.Assets {
			if strings.Contains(asset.Name, component) && strings.HasSuffix(asset.Name, ".mender") {
				menderAsset = asset.Name
				assetURL = asset.BrowserDownloadURL
				u.logger.Printf("Found .mender asset: %s with URL: %s", menderAsset, assetURL)
				break
			}
		}

		u.logger.Printf("Found release for component %s: %s (asset: %s)",
			component, release.TagName, menderAsset)

		// Check if update is needed
		isNeeded := u.isUpdateNeeded(component, release)
		if isNeeded {
			u.logger.Printf("Update needed for component %s", component)

			if assetURL != "" {
				u.logger.Printf("Adding update for component %s with URL %s", component, assetURL)
				updates = append(updates, updateInfo{
					component: component,
					release:   release,
					assetURL:  assetURL,
				})
			} else {
				u.logger.Printf("ERROR: No .mender asset URL found for component %s", component)
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

	// Special case for DBC: If no version is installed, turn on DBC and check again
	if currentVersion == "" && component == "dbc" {
		u.logger.Printf("No %s version found, turning on DBC to check version", component)
		
		// Send command to turn on DBC
		if err := u.redis.PushUpdateCommand("start-dbc"); err != nil {
			u.logger.Printf("Failed to send start-dbc command: %v", err)
			return true
		}
		
		// Wait for dashboard to be ready
		dashboardReady := u.waitForDashboardReady(30 * time.Second)
		if !dashboardReady {
			u.logger.Printf("Dashboard not ready after timeout, assuming update needed")
			return true
		}
		
		// Check version again
		currentVersion, err = u.redis.GetComponentVersion(component)
		if err != nil {
			u.logger.Printf("Failed to get current %s version after DBC startup: %v", component, err)
			return true
		}
		
		// If still no version, update is needed
		if currentVersion == "" {
			u.logger.Printf("No %s version found after DBC startup, update needed", component)
			return true
		}
		
		// Extract the timestamp part from the release tag
		parts := strings.Split(release.TagName, "-")
		if len(parts) < 2 {
			u.logger.Printf("Invalid release tag format: %s", release.TagName)
			return true
		}
		
		// Convert to lowercase for comparison with Redis version_id
		normalizedReleaseVersion := strings.ToLower(parts[1])
		
		// Check if update is needed
		if currentVersion != normalizedReleaseVersion {
			u.logger.Printf("Update needed for %s: current=%s, release=%s", component, currentVersion, normalizedReleaseVersion)
			return true
		}
		
		// No update needed, turn off DBC
		u.logger.Printf("No update needed for %s: current=%s, release=%s", component, currentVersion, normalizedReleaseVersion)
		if err := u.redis.PushUpdateCommand("complete-dbc"); err != nil {
			u.logger.Printf("Failed to send complete-dbc command to turn off DBC: %v", err)
		}
		
		return false
	}

	// Standard case: If no version is installed, an update is needed
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
	u.logger.Printf("Initiating update for component %s with release %s", component, release.TagName)

	// First check if updates are already in progress
	if u.hasUpdatesInProgress() {
		u.logger.Printf("CAUTION: Updates already in progress, will attempt to handle this update anyway")
	}

	// Find the .mender asset for the component
	var assetURL string
	u.logger.Printf("Looking for .mender asset in release assets")
	for _, asset := range release.Assets {
		if strings.Contains(asset.Name, component) && strings.HasSuffix(asset.Name, ".mender") {
			assetURL = asset.BrowserDownloadURL
			u.logger.Printf("Found asset URL: %s", assetURL)
			break
		}
	}

	if assetURL == "" {
		u.logger.Printf("ERROR: No .mender asset found for component %s", component)
		return fmt.Errorf("no .mender asset found for component %s", component)
	}

	// Update state will be set in Redis through the component status update
	u.logger.Printf("Component %s starting update", component)

	// Set update-type and update-component in Redis
	u.logger.Printf("Setting update-type to %s in Redis", UpdateTypeDownloading)
	if err := u.redis.SetOTAStatus(config.OtaStatusHashKey, "update-type", UpdateTypeDownloading); err != nil {
		u.logger.Printf("Failed to set update-type: %v", err)
	}

	u.logger.Printf("Setting update-component to %s in Redis", component)
	if err := u.redis.SetOTAStatus(config.OtaStatusHashKey, "update-component", component); err != nil {
		u.logger.Printf("Failed to set update-component: %v", err)
	}

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
	u.logger.Printf("Starting DBC update process with URL: %s", assetURL)

	// Check if it's safe to update DBC
	u.logger.Printf("Checking if safe to update DBC")
	safe, err := u.vehicle.IsSafeForDbcUpdate()
	if err != nil {
		u.logger.Printf("Error checking if safe for DBC update: %v", err)
		return fmt.Errorf("failed to check if safe for DBC update: %w", err)
	}

	if !safe {
		u.logger.Printf("Not safe to update DBC, scheduling retry")
		// This never actually happens, we just don't reboot the DBC
		return fmt.Errorf("not safe to update DBC")
	}
	u.logger.Printf("Safe to update DBC, proceeding")

	// Notify vehicle service that DBC update is starting
	u.logger.Printf("Sending start-dbc command to vehicle service")
	if err := u.redis.PushUpdateCommand("start-dbc"); err != nil {
		u.logger.Printf("Error sending start-dbc command: %v", err)
		return fmt.Errorf("failed to send start-dbc command: %w", err)
	}
	u.logger.Printf("Successfully sent start-dbc command")

	// Push update URL to SMUT
	u.logger.Printf("Pushing DBC update URL to Redis key %s: %s", u.config.DbcUpdateKey, assetURL)
	if err := u.redis.PushUpdateURL(u.config.DbcUpdateKey, assetURL); err != nil {
		u.logger.Printf("ERROR: Failed to push DBC update URL: %v", err)
		// Notify vehicle service that DBC update is complete (failed)
		u.logger.Printf("Sending complete-dbc command to indicate failure")
		u.redis.PushUpdateCommand("complete-dbc")
		return fmt.Errorf("failed to push DBC update URL: %w", err)
	}
	u.logger.Printf("Successfully pushed DBC update URL to Redis")

	return nil
}

// updateMDB updates the MDB component
func (u *Updater) updateMDB(assetURL string) error {
	u.logger.Printf("Starting MDB update process with URL: %s", assetURL)

	// Notify vehicle service that update is starting
	u.logger.Printf("Sending start command to vehicle service")
	if err := u.redis.PushUpdateCommand("start"); err != nil {
		u.logger.Printf("Error sending start command: %v", err)
		return fmt.Errorf("failed to send start command: %w", err)
	}
	u.logger.Printf("Successfully sent start command")

	// Push update URL to SMUT
	u.logger.Printf("Pushing MDB update URL to Redis key %s: %s", u.config.MdbUpdateKey, assetURL)
	if err := u.redis.PushUpdateURL(u.config.MdbUpdateKey, assetURL); err != nil {
		u.logger.Printf("ERROR: Failed to push MDB update URL: %v", err)
		// Notify vehicle service that update is complete (failed)
		u.logger.Printf("Sending complete command to indicate failure")
		u.redis.PushUpdateCommand("complete")
		return fmt.Errorf("failed to push MDB update URL: %w", err)
	}
	u.logger.Printf("Successfully pushed MDB update URL to Redis")

	return nil
}

// waitForMdbReboot waits for the MDB to be safe to reboot
func (u *Updater) waitForMdbReboot() {
	if u.vehicle.WaitForSafeReboot() {
		u.logger.Printf("Safe to reboot MDB now")

		// Update state to rebooting
		if err := u.redis.SetOTAStatus(config.OtaStatusHashKey, "update-type", UpdateTypeRebooting); err != nil {
			u.logger.Printf("Failed to set update-type to rebooting: %v", err)
		}

		if err := u.vehicle.TriggerReboot("mdb"); err != nil {
			u.logger.Printf("Failed to trigger MDB reboot: %v", err)

			// Update OTA status to indicate reboot failure
			if errStatus := u.redis.SetOTAStatus(config.OtaStatusHashKey, "status:mdb", "waiting-for-reboot"); errStatus != nil {
				u.logger.Printf("Failed to set MDB reboot-failed status: %v", errStatus)
			}
		} else {
			// Update OTA status to indicate reboot triggered
			if err := u.redis.SetOTAStatus(config.OtaStatusHashKey, "status:mdb", "rebooting"); err != nil {
				u.logger.Printf("Failed to set MDB rebooting status: %v", err)
			}
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
// Follows flow: Download DBC → Start MDB → Serve DBC → Complete DBC → Complete MDB
func (u *Updater) processUpdatesSequentially(mdbUpdate, dbcUpdate *updateInfo) {
	u.logger.Printf("Starting processUpdatesSequentially - mdbUpdate: %v, dbcUpdate: %v",
		mdbUpdate != nil, dbcUpdate != nil)

	if activeUpdates := u.hasUpdatesInProgress(); activeUpdates {
		u.logger.Printf("Updates already in progress, skipping this update cycle")
		return
	}

	// PHASE 1: Download DBC update to MDB (if needed)
	dbcLocalURL := ""
	if dbcUpdate != nil {
		u.logger.Printf("PHASE 1: Downloading DBC update to MDB filesystem")
		
		// Set DBC status to downloading
		if err := u.redis.SetOTAStatus(config.OtaStatusHashKey, "status:dbc", "downloading"); err != nil {
			u.logger.Printf("Failed to set DBC status to downloading: %v", err)
		}

		var err error
		dbcLocalURL, err = u.setupLocalUpdateServer(dbcUpdate.assetURL)
		if err != nil {
			u.logger.Printf("Failed to set up local update server for DBC: %v", err)
			// Use original URL if local server setup fails
			dbcLocalURL = ""
		} else {
			u.logger.Printf("DBC update downloaded and ready at local URL: %s", dbcLocalURL)
		}
		
		// Mark DBC download complete
		if err := u.redis.SetOTAStatus(config.OtaStatusHashKey, "status:dbc", ""); err != nil {
			u.logger.Printf("Failed to clear DBC downloading status: %v", err)
		}
	}

	// PHASE 2: Start MDB update (parallel with DBC serving)
	if mdbUpdate != nil {
		u.logger.Printf("PHASE 2: Starting MDB update")
		
		// Set MDB status to downloading/installing
		if err := u.redis.SetOTAStatus(config.OtaStatusHashKey, "status:mdb", "downloading"); err != nil {
			u.logger.Printf("Failed to set MDB status: %v", err)
		}
		
		if err := u.initiateUpdate(mdbUpdate.component, mdbUpdate.release); err != nil {
			u.logger.Printf("Failed to initiate MDB update: %v", err)
			if err := u.redis.SetOTAStatus(config.OtaStatusHashKey, "status:mdb", "failed"); err != nil {
				u.logger.Printf("Failed to set MDB status to failed: %v", err)
			}
		} else {
			u.logger.Printf("MDB update initiated successfully")
		}
	}

	// PHASE 3: Serve DBC update and let DBC install
	if dbcUpdate != nil {
		u.logger.Printf("PHASE 3: Serving DBC update and initiating DBC installation")
		
		// Ensure dashboard power is on
		if err := u.redis.PushUpdateCommand("start-dbc"); err != nil {
			u.logger.Printf("Failed to send start-dbc command: %v", err)
			return
		}
		
		// Wait for dashboard readiness
		if !u.waitForDashboardReady(30 * time.Second) {
			u.logger.Printf("Dashboard not ready, proceeding anyway")
		}
		
		// Determine URL to use
		urlToUse := dbcUpdate.assetURL
		if dbcLocalURL != "" {
			urlToUse = dbcLocalURL
		}
		
		// Update the asset URL and initiate DBC update
		dbcUpdate.assetURL = urlToUse
		if err := u.initiateUpdate(dbcUpdate.component, dbcUpdate.release); err != nil {
			u.logger.Printf("Failed to initiate DBC update: %v", err)
			if err := u.redis.SetOTAStatus(config.OtaStatusHashKey, "status:dbc", "failed"); err != nil {
				u.logger.Printf("Failed to set DBC status to failed: %v", err)
			}
		} else {
			u.logger.Printf("DBC update initiated successfully")
		}
	}

	// PHASE 4: Wait for DBC completion, then handle MDB reboot
	if dbcUpdate != nil {
		u.logger.Printf("PHASE 4: Waiting for DBC update completion")
		// The rest is handled by the OTA status message handlers
		// DBC will power down when safe, MDB will reboot after DBC is complete
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

	// Use /data/ota directory for downloaded files
	downloadDir := "/data/ota"

	// Ensure the directory exists
	if err := os.MkdirAll(downloadDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create download directory: %w", err)
	}

	// Extract filename from the remote URL
	urlParts := strings.Split(remoteURL, "/")
	fileName := urlParts[len(urlParts)-1]
	if fileName == "" {
		fileName = "dbc-update.mender"
	}

	// Download the DBC update file
	filePath := filepath.Join(downloadDir, fileName)
	u.logger.Printf("Downloading DBC update to: %s", filePath)

	// Create download ready channel and error variable
	downloadReady := make(chan struct{})
	var downloadErr error

	go func() {
		if err := u.downloadFile(remoteURL, filePath); err != nil {
			u.logger.Printf("Failed to download DBC update: %v", err)
			downloadErr = err
		} else {
			u.dbcUpdateFile = filePath
			u.logger.Printf("DBC update file downloaded successfully")
		}

		// Signal that the download is complete (whether successful or not)
		close(downloadReady)
	}()

	// Wait for download to complete or timeout
	select {
	case <-downloadReady:
		if downloadErr != nil {
			return "", fmt.Errorf("download failed: %w", downloadErr)
		}
	case <-time.After(5 * time.Minute):
		u.logger.Printf("Timed out waiting for DBC update download")
		return "", fmt.Errorf("download timeout exceeded")
	}

	// Set up the HTTP server to serve the downloaded file
	mux := http.NewServeMux()
	mux.HandleFunc("/ota/", func(w http.ResponseWriter, r *http.Request) {
		u.logger.Printf("Serving DBC update file: %s", filePath)

		// Check if file exists before serving
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			u.logger.Printf("Error: Update file not found: %s", filePath)
			http.Error(w, "Update file not found", http.StatusNotFound)
			return
		}

		// Set appropriate headers
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", fileName))
		w.Header().Set("Content-Type", "application/octet-stream")

		// Serve the file
		http.ServeFile(w, r, filePath)
	})

	// Start the HTTP server
	u.httpServerMutex.Lock()
	defer u.httpServerMutex.Unlock()

	// Create server with reasonable timeouts
	u.httpServer = &http.Server{
		Addr:         ":8000",
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
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

	// Write the body to file
	_, err = io.Copy(out, resp.Body)
	if err != nil {
		return fmt.Errorf("failed to copy content: %w", err)
	}

	return nil
}

