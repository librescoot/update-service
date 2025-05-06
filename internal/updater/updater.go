package updater

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/librescoot/update-service/internal/config"
	"github.com/librescoot/update-service/internal/redis"
	"github.com/librescoot/update-service/internal/vehicle"
)

// Updater represents the update orchestrator
type Updater struct {
	config      *config.Config
	redis       *redis.Client
	vehicle     *vehicle.Service
	logger      *log.Logger
	ctx         context.Context
	cancel      context.CancelFunc
	githubAPI   *GitHubAPI
	updateState map[string]string
	stateMutex  sync.Mutex
}

// New creates a new updater
func New(ctx context.Context, cfg *config.Config, redisClient *redis.Client, vehicleService *vehicle.Service, logger *log.Logger) *Updater {
	updaterCtx, cancel := context.WithCancel(ctx)
	return &Updater{
		config:      cfg,
		redis:       redisClient,
		vehicle:     vehicleService,
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

	// Start the update check loop
	go u.updateCheckLoop()

	return nil
}

// Stop stops the updater
func (u *Updater) Stop() {
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

	// Process releases for each component and channel
	for _, component := range u.config.Components {
		// Get the latest release for the component and channel
		release, found := u.findLatestRelease(releases, component, u.config.DefaultChannel)
		if !found {
			u.logger.Printf("No release found for component %s and channel %s", component, u.config.DefaultChannel)
			continue
		}

		// Find the .mender asset for the component
		var menderAsset string
		for _, asset := range release.Assets {
			if strings.Contains(asset.Name, component) && strings.HasSuffix(asset.Name, ".mender") {
				menderAsset = asset.Name
				break
			}
		}
		
		u.logger.Printf("Found release for component %s: %s (asset: %s)", component, release.TagName, menderAsset)

		// Check if update is needed
		if u.isUpdateNeeded(component, release) {
			u.logger.Printf("Update needed for component %s", component)

			// Initiate update
			if err := u.initiateUpdate(component, release); err != nil {
				u.logger.Printf("Failed to initiate update for component %s: %v", component, err)
				continue
			}
		} else {
			u.logger.Printf("No update needed for component %s", component)
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
	// TODO: Implement proper version checking
	// For now, always return true for testing
	return true
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
		// TODO: Implement retry logic
		return fmt.Errorf("not safe to update DBC")
	}

	// Set vehicle state to "updating"
	if err := u.vehicle.SetUpdatingState(); err != nil {
		return fmt.Errorf("failed to set vehicle state to updating: %w", err)
	}

	// Push update URL to SMUT
	u.logger.Printf("Pushing DBC update URL to Redis key %s: %s", u.config.DbcUpdateKey, assetURL)
	if err := u.redis.PushUpdateURL(u.config.DbcUpdateKey, assetURL); err != nil {
		// Restore vehicle state
		u.vehicle.RestorePreviousState()
		return fmt.Errorf("failed to push DBC update URL: %w", err)
	}

	// Note: We don't set the checksum as we don't have it

	// Monitor update progress
	go u.monitorUpdate("dbc")

	return nil
}

// updateMDB updates the MDB component
func (u *Updater) updateMDB(assetURL string) error {
	// Push update URL to SMUT
	u.logger.Printf("Pushing MDB update URL to Redis key %s: %s", u.config.MdbUpdateKey, assetURL)
	if err := u.redis.PushUpdateURL(u.config.MdbUpdateKey, assetURL); err != nil {
		return fmt.Errorf("failed to push MDB update URL: %w", err)
	}

	// Note: We don't set the checksum as we don't have it

	// Monitor update progress
	go u.monitorUpdate("mdb")

	return nil
}

// monitorUpdate monitors the update progress for the given component
func (u *Updater) monitorUpdate(component string) {
	u.logger.Printf("Monitoring update progress for %s", component)

	// Poll OTA status
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-u.ctx.Done():
			u.logger.Printf("Update monitoring stopped for %s", component)
			return
		case <-ticker.C:
			status, err := u.redis.GetOTAStatus(config.OtaStatusHashKey)
			if err != nil {
				u.logger.Printf("Failed to get OTA status: %v", err)
				continue
			}

			u.logger.Printf("OTA status for %s: %v", component, status)

			// Check if update is complete
			if status["status"] == "complete" {
				u.logger.Printf("Update complete for %s", component)
				u.stateMutex.Lock()
				u.updateState[component] = "complete"
				u.stateMutex.Unlock()

				// Handle post-update actions
				if component == "dbc" {
					// Restore vehicle state
					if err := u.vehicle.RestorePreviousState(); err != nil {
						u.logger.Printf("Failed to restore vehicle state: %v", err)
					}

					// Check if reboot is needed
					if status["reboot-needed"] == "true" {
						u.logger.Printf("Reboot needed for DBC")
						if err := u.vehicle.TriggerReboot("dbc"); err != nil {
							u.logger.Printf("Failed to trigger DBC reboot: %v", err)
						}
					}
				} else if component == "mdb" {
					// Check if reboot is needed
					if status["reboot-needed"] == "true" {
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

				return
			}

			// Check if update failed
			if status["status"] == "failed" {
				u.logger.Printf("Update failed for %s: %s", component, status["error"])
				u.stateMutex.Lock()
				u.updateState[component] = "failed"
				u.stateMutex.Unlock()

				// Restore vehicle state if DBC update
				if component == "dbc" {
					if err := u.vehicle.RestorePreviousState(); err != nil {
						u.logger.Printf("Failed to restore vehicle state: %v", err)
					}
				}

				return
			}
		}
	}
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
