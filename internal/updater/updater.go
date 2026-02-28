package updater

import (
	"context"
	"fmt"
	"log"
	"os"
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

	// Update method configuration
	updateMethodMu sync.RWMutex
	updateMethod   string

	// Prevent concurrent update checks
	updateCheckMu sync.Mutex
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
		status:    status.NewReporter(redisClient.GetClient(), cfg.Component, logger),
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

	return u
}

// CheckAndCommitPendingUpdate checks mender state and commits if needed.
// Returns true if an update is installed but waiting for reboot.
func (u *Updater) CheckAndCommitPendingUpdate() (needsReboot bool, err error) {
	// Get expected version from Redis (set during download/install)
	expectedVersion, _ := u.redis.GetTargetVersion(u.config.Component)

	state, err := u.mender.CheckUpdateState(expectedVersion)
	if err != nil {
		u.logger.Printf("Failed to check mender state: %v", err)
		return false, nil // Don't fail startup
	}

	switch state {
	case mender.StateNeedsCommit:
		// We've rebooted into the new partition - commit it
		u.logger.Printf("Committing pending update")
		if err := u.mender.Commit(); err != nil {
			u.logger.Printf("Failed to commit update: %v", err)
			return false, err
		}
		u.logger.Printf("Update committed successfully")
		return false, nil

	case mender.StateNeedsReboot:
		// Update installed but not yet rebooted
		return true, nil

	case mender.StateInconsistent:
		u.logger.Printf("Mender in inconsistent state, may need manual intervention")
		return false, nil

	default:
		return false, nil
	}
}

// Start starts the updater. The menderNeedsReboot parameter indicates if
// CheckAndCommitPendingUpdate detected that mender has an update waiting for reboot.
func (u *Updater) Start(menderNeedsReboot bool) error {
	// Recover from any stuck status on startup
	if err := u.recoverFromStuckState(menderNeedsReboot); err != nil {
		u.logger.Printf("Warning: Failed to recover from stuck state: %v", err)
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

// recoverFromStuckState recovers from any stuck status on startup.
// The menderNeedsReboot parameter indicates if mender has an update waiting for reboot.
func (u *Updater) recoverFromStuckState(menderNeedsReboot bool) error {
	currentStatus, err := u.status.GetStatus(u.ctx)
	if err != nil {
		return fmt.Errorf("failed to get current status: %w", err)
	}

	// Nothing to recover if already idle or empty
	if currentStatus == status.StatusIdle || currentStatus == "" {
		return nil
	}

	u.logger.Printf("Found %s status for %s on startup", currentStatus, u.config.Component)

	switch currentStatus {
	case status.StatusDownloading:
		// Check if file exists for logging purposes
		targetVersion, _ := u.redis.GetTargetVersion(u.config.Component)
		if targetVersion != "" {
			if filePath, exists := u.mender.FindMenderFileForVersion(targetVersion); exists {
				u.logger.Printf("Found complete file %s, next check will resume install", filePath)
			}
		}

		// Clean up stale temporary directories from interrupted delta applications
		// The mender-apply-delta.py script creates temp dirs in /data/ota/tmp/
		if err := u.cleanupDeltaTempDirs(); err != nil {
			u.logger.Printf("Warning: Failed to cleanup delta temp dirs: %v", err)
		}

		u.logger.Printf("Clearing downloading status")
		if err := u.status.SetIdleAndClearVersion(u.ctx); err != nil {
			return fmt.Errorf("failed to clear downloading status: %w", err)
		}

	case status.StatusInstalling:
		if menderNeedsReboot {
			// Mender install completed but needs reboot - transition to rebooting
			u.logger.Printf("Mender install complete, setting status to rebooting")
			if err := u.status.SetStatus(u.ctx, status.StatusRebooting); err != nil {
				return fmt.Errorf("failed to set rebooting status: %w", err)
			}
		} else {
			// Mender install failed or was interrupted
			u.logger.Printf("Clearing installing status (mender has no pending update)")
			if err := u.status.SetIdleAndClearVersion(u.ctx); err != nil {
				return fmt.Errorf("failed to clear installing status: %w", err)
			}
		}

	case status.StatusRebooting:
		// Reboot happened - commit was already attempted by CheckAndCommitPendingUpdate
		u.logger.Printf("Clearing rebooting status (reboot completed)")
		if err := u.status.SetIdleAndClearVersion(u.ctx); err != nil {
			return fmt.Errorf("failed to clear rebooting status: %w", err)
		}

		// For DBC component, notify vehicle-service that update is complete
		// (the defer that normally sends this didn't execute due to reboot)
		if u.config.Component == "dbc" {
			u.logger.Printf("Sending complete-dbc command after reboot")
			if err := u.redis.PushUpdateCommand("complete-dbc"); err != nil {
				u.logger.Printf("Failed to send complete-dbc command: %v", err)
			}
		}

	case status.StatusError:
		u.logger.Printf("Clearing error status to allow retry")
		if err := u.status.SetIdleAndClearVersion(u.ctx); err != nil {
			return fmt.Errorf("failed to clear error status: %w", err)
		}
	}

	return nil
}

// cleanupDeltaTempDirs removes stale temporary directories created by mender-apply-delta.py
// The script creates temp dirs in /data/ota/tmp/ that need cleanup after interruptions
func (u *Updater) cleanupDeltaTempDirs() error {
	tmpDir := "/data/ota/tmp"

	// Check if tmp directory exists
	if _, err := os.Stat(tmpDir); os.IsNotExist(err) {
		return nil // Nothing to clean
	} else if err != nil {
		return fmt.Errorf("failed to stat tmp directory: %w", err)
	}

	// Read all entries in the tmp directory
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return fmt.Errorf("failed to read tmp directory: %w", err)
	}

	// Remove all temp directories (created by Python's tempfile.TemporaryDirectory)
	for _, entry := range entries {
		if entry.IsDir() {
			dirPath := filepath.Join(tmpDir, entry.Name())
			u.logger.Printf("Removing stale delta temp directory: %s", dirPath)
			if err := os.RemoveAll(dirPath); err != nil {
				u.logger.Printf("Warning: Failed to remove temp directory %s: %v", dirPath, err)
			}
		}
	}

	return nil
}

// initializeRedisKeys ensures Redis keys for this component are initialized
func (u *Updater) initializeRedisKeys() error {
	// Check if status key exists and get its value
	statusKey := fmt.Sprintf("status:%s", u.config.Component)
	statusValue, err := u.redis.GetOTAField("ota", statusKey)

	// Initialize if key doesn't exist or if it's empty
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
	currentState, _, err := u.redis.GetVehicleStateWithTimestamp(config.VehicleHashKey)
	if err != nil {
		u.logger.Printf("Vehicle state: unknown (%v)", err)
		return
	}

	if currentState == "stand-by" {
		// Use ota:standby-timer-start as the authoritative source (set by vehicle-service on standby entry)
		standbyTimerStart, err := u.redis.GetStandbyTimerStart()
		if err == nil && !standbyTimerStart.IsZero() {
			u.standbyStartTime = standbyTimerStart
			elapsed := time.Since(standbyTimerStart)
			u.logger.Printf("Vehicle in 'stand-by' since %s (%v ago)", standbyTimerStart.Format(time.RFC3339), elapsed)
		} else {
			// Fallback to current time if standby-timer-start is not set
			u.standbyStartTime = time.Now()
			u.logger.Printf("Vehicle in 'stand-by' (no timer-start timestamp, using current time)")
		}
	} else {
		u.logger.Printf("Vehicle state: %s (will wait for stand-by before reboot)", currentState)
	}
}

// revalidateStandbyState re-validates the vehicle state after long-running operations
// Uses ota:standby-timer-start as the authoritative source (set by vehicle-service on standby entry).
// If vehicle stayed in standby, the timer-start will be unchanged and we preserve the original time.
// If vehicle left and re-entered standby, vehicle-service will have updated timer-start.
func (u *Updater) revalidateStandbyState() {
	currentState, _, err := u.redis.GetVehicleStateWithTimestamp(config.VehicleHashKey)
	if err != nil {
		u.logger.Printf("Failed to get vehicle state after long-running operation: %v (clearing standby timestamp)", err)
		u.standbyStartTime = time.Time{} // Clear on error to be safe
		return
	}

	if currentState == "stand-by" {
		// Use ota:standby-timer-start as the authoritative source
		standbyTimerStart, err := u.redis.GetStandbyTimerStart()
		if err == nil && !standbyTimerStart.IsZero() {
			elapsed := time.Since(standbyTimerStart)
			if standbyTimerStart.Equal(u.standbyStartTime) {
				u.logger.Printf("Vehicle still in 'stand-by' since %s (%v elapsed) - keeping original timestamp", standbyTimerStart.Format(time.RFC3339), elapsed)
			} else {
				u.standbyStartTime = standbyTimerStart
				u.logger.Printf("Vehicle re-entered 'stand-by' at %s (%v ago)", standbyTimerStart.Format(time.RFC3339), elapsed)
			}
		} else {
			// No standby-timer-start but vehicle is in standby - use current time
			u.standbyStartTime = time.Now()
			u.logger.Printf("Vehicle in 'stand-by' after operation (no timer-start) - using current time")
		}
	} else {
		u.logger.Printf("Vehicle not in 'stand-by' after operation (current: %s) - clearing standby timestamp", currentState)
		u.standbyStartTime = time.Time{}
	}
}

// listenForCommands listens for Redis commands on scooter:update:{component}
func (u *Updater) listenForCommands() {
	// Use redis-ipc HandleRequests to process commands
	u.redis.HandleUpdateCommands(u.config.Component, func(command string) error {
		u.handleCommand(command)
		return nil
	})
}

// handleCommand handles incoming Redis commands
func (u *Updater) handleCommand(command string) {
	switch {
	case command == "check-now":
		u.logger.Printf("Received check-now command, triggering immediate update check")
		go u.checkForUpdates()

	case strings.HasPrefix(command, "update-from-file:"):
		filePath := strings.TrimPrefix(command, "update-from-file:")
		u.logger.Printf("Received update-from-file command: %s", filePath)
		go func() {
			u.handleUpdateFromFile(filePath)
		}()

	case strings.HasPrefix(command, "update-from-url:"):
		url := strings.TrimPrefix(command, "update-from-url:")
		u.logger.Printf("Received update-from-url command: %s", url)
		go func() {
			u.handleUpdateFromURL(url)
		}()

	default:
		u.logger.Printf("Unknown update command: %s", command)
	}
}

// parseUpdateSource parses an update source (file path or URL) and extracts optional checksum
// Returns: (source, checksum, isURL)
func (u *Updater) parseUpdateSource(source string) (string, string, bool) {
	parts := strings.SplitN(source, ":", 3)

	if len(parts) >= 2 && (parts[0] == "http" || parts[0] == "https" || parts[0] == "file") {
		if len(parts) == 3 && parts[1] == "sha256" {
			return parts[0] + ":" + parts[1], parts[2], true
		}
		return source, "", true
	}

	if len(parts) == 3 && parts[1] == "sha256" {
		return parts[0], parts[2], false
	}

	return source, "", false
}

// isURL checks if the given string is a URL
func (u *Updater) isURL(source string) bool {
	return strings.HasPrefix(source, "http://") ||
		strings.HasPrefix(source, "https://") ||
		strings.HasPrefix(source, "file://")
}

// handleUpdateFromFile processes an update from a local file path
// Supports format: /path/to/file.mender or /path/to/file.mender:sha256:checksum
func (u *Updater) handleUpdateFromFile(filePath string) {
	source, _, _ := u.parseUpdateSource(filePath)
	isURL := u.isURL(filePath)

	if isURL {
		u.handleUpdateFromURL(filePath)
		return
	}

	u.logger.Printf("Processing update from local file: %s", source)

	if _, err := os.Stat(source); err != nil {
		u.logger.Printf("Error: file not found: %s", source)
		if err := u.status.SetError(u.ctx, "file-not-found", fmt.Sprintf("File not found: %s", source)); err != nil {
			u.logger.Printf("Failed to set error status: %v", err)
		}
		return
	}

	if !strings.HasSuffix(source, ".mender") {
		u.logger.Printf("Error: file is not a .mender file: %s", source)
		if err := u.status.SetError(u.ctx, "invalid-file", fmt.Sprintf("File is not a .mender file: %s", source)); err != nil {
			u.logger.Printf("Failed to set error status: %v", err)
		}
		return
	}

	u.logger.Printf("File exists: %s", source)

	var version string
	filename := filepath.Base(source)
	filename = strings.TrimSuffix(filename, ".mender")

	if strings.Contains(filename, "nightly-") {
		version = strings.ToLower(filename)
	} else if strings.Contains(filename, "testing-") {
		version = strings.ToLower(filename)
	} else if strings.HasPrefix(filename, "v") {
		version = filename
	} else {
		version = filename
	}

	u.logger.Printf("Detected version from filename: %s", version)

	if err := u.status.SetStatusAndVersion(u.ctx, status.StatusDownloading, version); err != nil {
		u.logger.Printf("Failed to set downloading status: %v", err)
		return
	}

	u.logger.Printf("Setting update method to full for file update")
	if err := u.status.SetUpdateMethod(u.ctx, "full"); err != nil {
		u.logger.Printf("Failed to set update method: %v", err)
	}

	// For DBC updates, notify vehicle-service to keep dashboard power on
	if u.config.Component == "dbc" {
		u.logger.Printf("Starting DBC file update - sending start-dbc command")
		if err := u.redis.PushUpdateCommand("start-dbc"); err != nil {
			u.logger.Printf("Failed to send start-dbc command: %v", err)
		}
	}

	u.logger.Printf("File ready for installation: %s", source)

	if err := u.inhibitor.AddDownloadInhibit(u.config.Component); err != nil {
		u.logger.Printf("Failed to add download inhibit: %v", err)
	}

	defer func() {
		if err := u.inhibitor.RemoveDownloadInhibit(u.config.Component); err != nil {
			u.logger.Printf("Failed to remove download inhibit: %v", err)
		}
		if err := u.inhibitor.RemoveInstallInhibit(u.config.Component); err != nil {
			u.logger.Printf("Failed to remove install inhibit: %v", err)
		}

		if u.config.Component == "dbc" {
			u.logger.Printf("File update cleanup - sending complete-dbc command")
			if err := u.redis.PushUpdateCommand("complete-dbc"); err != nil {
				u.logger.Printf("Failed to send complete-dbc command: %v", err)
			}
		}
	}()

	if err := u.status.SetStatus(u.ctx, status.StatusInstalling); err != nil {
		u.logger.Printf("Failed to set installing status: %v", err)
		return
	}

	if err := u.inhibitor.AddInstallInhibit(u.config.Component); err != nil {
		u.logger.Printf("Failed to add install inhibit: %v", err)
	}

	if err := u.inhibitor.RemoveDownloadInhibit(u.config.Component); err != nil {
		u.logger.Printf("Failed to remove download inhibit: %v", err)
	}

	if err := u.mender.Install(source); err != nil {
		u.logger.Printf("Failed to install update: %v", err)
		if err := u.status.SetError(u.ctx, "install-failed", fmt.Sprintf("Failed to install update: %v", err)); err != nil {
			u.logger.Printf("Failed to set error status: %v", err)
		}
		return
	}

	u.logger.Printf("Successfully installed update from file")

	if err := u.status.SetStatus(u.ctx, status.StatusRebooting); err != nil {
		u.logger.Printf("Failed to set rebooting status: %v", err)
	}

	if err := u.inhibitor.RemoveInstallInhibit(u.config.Component); err != nil {
		u.logger.Printf("Failed to remove install inhibit: %v", err)
	}

	u.logger.Printf("Update installation complete, system will reboot to apply changes")

	if u.config.Component == "mdb" || u.config.Component == "dbc" {
		u.logger.Printf("%s update installed, triggering reboot", strings.ToUpper(u.config.Component))
		err := u.TriggerReboot(u.config.Component)
		if err != nil {
			u.logger.Printf("Failed to trigger %s reboot: %v", u.config.Component, err)
			if !strings.Contains(err.Error(), "DRY-RUN") {
				if statusErr := u.status.SetError(u.ctx, "reboot-failed", fmt.Sprintf("Failed to trigger %s reboot: %v", u.config.Component, err)); statusErr != nil {
					u.logger.Printf("Additionally failed to set error status after %s reboot trigger failure: %v", u.config.Component, statusErr)
				}
			}

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

// handleUpdateFromURL processes an update from a URL
// Supports format: https://example.com/file.mender or https://example.com/file.mender:sha256:checksum
func (u *Updater) handleUpdateFromURL(url string) {
	source, checksum, _ := u.parseUpdateSource(url)
	if checksum != "" {
		u.logger.Printf("Checksum provided: %s", checksum)
	}

	u.logger.Printf("Processing update from URL: %s", source)

	if err := u.status.SetStatusAndVersion(u.ctx, status.StatusDownloading, "unknown"); err != nil {
		u.logger.Printf("Failed to set downloading status: %v", err)
		return
	}

	u.logger.Printf("Setting update method based on configuration")
	updateMethod := u.getUpdateMethod()
	if err := u.status.SetUpdateMethod(u.ctx, updateMethod); err != nil {
		u.logger.Printf("Failed to set update method: %v", err)
	}

	for i := 0; i < 5; i++ {
		if u.ctx.Err() != nil {
			u.logger.Printf("Context cancelled during download attempt %d", i+1)
			return
		}

		u.logger.Printf("Starting download attempt %d/5", i+1)

		filePath, err := u.mender.DownloadAndVerify(u.ctx, source, checksum, func(downloaded, total int64) {
			if err := u.status.SetDownloadProgress(u.ctx, downloaded, total); err != nil {
				u.logger.Printf("Failed to update download progress: %v", err)
			}
		})

		if err != nil {
			u.logger.Printf("Download failed (attempt %d/5): %v", i+1, err)
			if i < 4 {
				sleepTime := time.Duration(1<<uint(i)) * time.Second
				u.logger.Printf("Waiting %v before retry...", sleepTime)
				select {
				case <-u.ctx.Done():
					return
				case <-time.After(sleepTime):
				}
				continue
			}
			u.logger.Printf("Failed to download update after 5 attempts: %v", err)
			if err := u.status.SetError(u.ctx, "download-failed", fmt.Sprintf("Failed to download update: %v", err)); err != nil {
				u.logger.Printf("Failed to set error status: %v", err)
			}
			return
		}

		u.logger.Printf("Successfully downloaded update to: %s", filePath)

		if err := u.status.ClearDownloadProgress(u.ctx); err != nil {
			u.logger.Printf("Failed to clear download progress: %v", err)
		}

		u.revalidateStandbyState()

		if err := u.status.SetStatus(u.ctx, status.StatusInstalling); err != nil {
			u.logger.Printf("Failed to set installing status: %v", err)
			return
		}

		if err := u.inhibitor.AddInstallInhibit(u.config.Component); err != nil {
			u.logger.Printf("Failed to add install inhibit: %v", err)
		}

		if err := u.inhibitor.RemoveDownloadInhibit(u.config.Component); err != nil {
			u.logger.Printf("Failed to remove download inhibit: %v", err)
		}

		if err := u.mender.Install(filePath); err != nil {
			u.logger.Printf("Failed to install update: %v", err)

			errStr := err.Error()
			isCorruptionError := strings.Contains(errStr, "gzip") ||
				strings.Contains(errStr, "checksum") ||
				strings.Contains(errStr, "corrupt") ||
				strings.Contains(errStr, "truncated")

			if isCorruptionError {
				u.logger.Printf("Installation failed due to file corruption, deleting corrupted file: %s", filePath)
				if removeErr := u.mender.RemoveFile(filePath); removeErr != nil {
					u.logger.Printf("Warning: Failed to delete corrupted file: %v", removeErr)
				} else {
					u.logger.Printf("Deleted corrupted file, next update check will re-download")
				}
			}

			if err := u.status.SetError(u.ctx, "install-failed", fmt.Sprintf("Failed to install update: %v", err)); err != nil {
				u.logger.Printf("Failed to set error status: %v", err)
			}
			return
		}

		u.logger.Printf("Successfully installed update")

		if err := u.status.SetStatus(u.ctx, status.StatusRebooting); err != nil {
			u.logger.Printf("Failed to set rebooting status: %v", err)
		}

		if err := u.inhibitor.RemoveInstallInhibit(u.config.Component); err != nil {
			u.logger.Printf("Failed to remove install inhibit: %v", err)
		}

		u.logger.Printf("Update installation complete, system will reboot to apply changes")

		if u.config.Component == "mdb" || u.config.Component == "dbc" {
			u.logger.Printf("%s update installed, triggering reboot", strings.ToUpper(u.config.Component))
			err := u.TriggerReboot(u.config.Component)
			if err != nil {
				u.logger.Printf("Failed to trigger %s reboot: %v", u.config.Component, err)
				if !strings.Contains(err.Error(), "DRY-RUN") {
					if statusErr := u.status.SetError(u.ctx, "reboot-failed", fmt.Sprintf("Failed to trigger %s reboot: %v", u.config.Component, err)); statusErr != nil {
						u.logger.Printf("Additionally failed to set error status after %s reboot trigger failure: %v", u.config.Component, statusErr)
					}
				}

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

		return
	}
}

// monitorSettingsChanges monitors Redis pub/sub for update method configuration changes
func (u *Updater) monitorSettingsChanges() {
	settingKey := fmt.Sprintf("updates.%s.method", u.config.Component)
	watcher := u.redis.NewSettingsWatcher()
	watcher.OnField(settingKey, func(value string) error {
		u.logger.Printf("Received settings change for %s: %s", settingKey, value)

		// Validate the method
		if value != "delta" && value != "full" {
			u.logger.Printf("Invalid update method value '%s', ignoring", value)
			return nil
		}

		// Update the cached value
		u.setUpdateMethod(value)
		return nil
	})

	if err := watcher.Start(); err != nil {
		u.logger.Printf("Failed to start settings watcher: %v", err)
		return
	}
	defer watcher.Stop()

	// Wait for context cancellation
	<-u.ctx.Done()
	u.logger.Printf("Settings monitor stopped for %s", u.config.Component)
}

// updateCheckLoop periodically checks for updates
func (u *Updater) updateCheckLoop() {
	if u.config.CheckInterval == 0 {
		<-u.ctx.Done()
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
	// Prevent concurrent update checks - if an update is already in progress, skip
	if !u.updateCheckMu.TryLock() {
		u.logger.Printf("Update check already in progress for %s, skipping duplicate request", u.config.Component)
		return
	}
	defer u.updateCheckMu.Unlock()

	u.logger.Printf("Checking for updates for component %s on channel %s", u.config.Component, u.config.Channel)

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

	if currentStatus != status.StatusIdle && currentStatus != "" {
		u.logger.Printf("Component %s is in %s state, deferring update check", u.config.Component, currentStatus)
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
	return u.redis.GetComponentVersion(u.config.Component)
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
	if errCommit := u.mender.Commit(); errCommit != nil {
		u.logger.Printf("Pending update commit failed (proceeding anyway): %v", errCommit)
	}

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

		// Check if this is a corruption error (gzip decompression, checksum failure, etc.)
		errStr := err.Error()
		isCorruptionError := strings.Contains(errStr, "gzip") ||
			strings.Contains(errStr, "checksum") ||
			strings.Contains(errStr, "corrupt") ||
			strings.Contains(errStr, "truncated")

		if isCorruptionError {
			u.logger.Printf("Installation failed due to file corruption, deleting corrupted file: %s", filePath)
			if removeErr := u.mender.RemoveFile(filePath); removeErr != nil {
				u.logger.Printf("Warning: Failed to delete corrupted file: %v", removeErr)
			} else {
				u.logger.Printf("Deleted corrupted file, next update check will re-download")
			}
		}

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

// deltaDownload represents a downloaded delta file
type deltaDownload struct {
	release   Release
	deltaPath string
	deltaURL  string
	err       error
}

// performDeltaUpdate attempts to apply a chain of delta updates to reach the latest version
func (u *Updater) performDeltaUpdate(releases []Release, currentVersion, variantID string) {
	// Step 0: Determine the base version to start from
	// First, check if we have a mender file for the current running version
	baseVersion := currentVersion
	if _, exists := u.mender.FindMenderFileForVersion(currentVersion); !exists {
		// No mender file for current version - find the latest mender file we have for this channel
		_, menderVersion, found := u.mender.FindLatestMenderFile(u.config.Channel)
		if !found || menderVersion == "" {
			u.logger.Printf("No mender file found for current version %s and no other mender files available for channel %s", currentVersion, u.config.Channel)
			u.logger.Printf("Delta updates require a base mender file - would fall back to full update (DISABLED FOR TESTING)")
			// TODO: Re-enable fallback after testing
			return
		}
		u.logger.Printf("No mender file for running version %s, using available mender file version %s as base", currentVersion, menderVersion)
		baseVersion = menderVersion
	}

	// Step 1: Build the delta chain from the base version
	deltaChain, err := u.buildDeltaChain(releases, baseVersion, u.config.Channel, variantID)
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

	if len(deltaChain) == 0 {
		u.logger.Printf("Already at latest version %s (base: %s)", currentVersion, baseVersion)
		return
	}

	// Get latest version from chain
	latestVersion := strings.ToLower(deltaChain[len(deltaChain)-1].TagName)

	// Calculate total delta size and compare with full update size
	var totalDeltaSize int64
	for _, release := range deltaChain {
		for _, asset := range release.Assets {
			if strings.Contains(asset.Name, variantID) && strings.HasSuffix(asset.Name, ".delta") {
				totalDeltaSize += asset.Size
				break
			}
		}
	}

	// Find full update size from the latest release
	latestRelease := deltaChain[len(deltaChain)-1]
	var fullUpdateSize int64
	for _, asset := range latestRelease.Assets {
		if strings.Contains(asset.Name, variantID) && strings.HasSuffix(asset.Name, ".mender") {
			fullUpdateSize = asset.Size
			break
		}
	}

	// If total delta size >= full update size, use full update instead
	if fullUpdateSize > 0 && totalDeltaSize >= fullUpdateSize {
		u.logger.Printf("Total delta size (%d bytes) >= full update size (%d bytes), using full update instead",
			totalDeltaSize, fullUpdateSize)
		menderURL := u.findMenderAsset(latestRelease, variantID)
		if menderURL != "" {
			u.performUpdate(latestRelease, menderURL)
		}
		return
	}

	if totalDeltaSize > 0 && fullUpdateSize > 0 {
		u.logger.Printf("Delta chain size: %d bytes, full update: %d bytes (saving %d bytes)",
			totalDeltaSize, fullUpdateSize, fullUpdateSize-totalDeltaSize)
	}

	// Log the delta chain
	u.logger.Printf("Built delta chain with %d updates: %s -> %s", len(deltaChain), baseVersion, latestVersion)
	for i, release := range deltaChain {
		u.logger.Printf("  Delta %d/%d: %s", i+1, len(deltaChain), release.TagName)
	}
	u.logger.Printf("Starting multi-delta update process for %s: %s -> %s (%d deltas)", u.config.Component, baseVersion, latestVersion, len(deltaChain))

	// Step 0: Check and commit any pending Mender update
	if err := u.mender.Commit(); err != nil {
		u.logger.Printf("Pending update commit failed (proceeding anyway): %v", err)
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

	// Progress callback for downloads (we'll track total across all deltas)
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

	// Step 2: Download all deltas upfront
	u.logger.Printf("=== Downloading all %d deltas ===", len(deltaChain))
	downloads := make([]deltaDownload, len(deltaChain))

	for i, release := range deltaChain {
		deltaURL := ""
		for _, asset := range release.Assets {
			if strings.Contains(asset.Name, variantID) && strings.HasSuffix(asset.Name, ".delta") {
				deltaURL = asset.BrowserDownloadURL
				break
			}
		}

		if deltaURL == "" {
			u.logger.Printf("No delta asset found for %s, cannot continue", release.TagName)
			u.fallbackToFullUpdate(releases, variantID, "no delta asset found")
			return
		}

		downloads[i].release = release
		downloads[i].deltaURL = deltaURL

		u.logger.Printf("Downloading delta %d/%d: %s", i+1, len(deltaChain), release.TagName)
		deltaPath, err := u.mender.DownloadDelta(u.ctx, deltaURL, downloadProgressCallback)
		if err != nil {
			u.logger.Printf("Failed to download delta %d/%d: %v", i+1, len(deltaChain), err)
			// Clean up any previously downloaded deltas
			for j := 0; j < i; j++ {
				if downloads[j].deltaPath != "" {
					u.mender.CleanupDeltaFile(downloads[j].deltaPath)
				}
			}
			u.fallbackToFullUpdate(releases, variantID, fmt.Sprintf("download failed: %v", err))
			return
		}
		downloads[i].deltaPath = deltaPath
		u.logger.Printf("Downloaded delta %d/%d to %s", i+1, len(deltaChain), deltaPath)

		// Clear download progress between deltas
		if err := u.status.ClearDownloadProgress(u.ctx); err != nil {
			u.logger.Printf("Failed to clear download progress: %v", err)
		}
	}

	u.logger.Printf("=== All deltas downloaded, applying sequentially ===")

	// Step 3: Apply all deltas sequentially
	var finalMenderPath string
	workingVersion := baseVersion

	for i, dl := range downloads {
		deltaNum := i + 1
		targetVersion := strings.ToLower(dl.release.TagName)

		u.logger.Printf("=== Applying delta %d/%d: %s -> %s ===", deltaNum, len(downloads), workingVersion, targetVersion)

		newMenderPath, err := u.mender.ApplyDownloadedDelta(u.ctx, dl.deltaPath, workingVersion, installProgressCallback)
		if err != nil {
			u.logger.Printf("Delta %d/%d failed: %v", deltaNum, len(downloads), err)
			// Clean up remaining downloaded deltas
			for j := i + 1; j < len(downloads); j++ {
				if downloads[j].deltaPath != "" {
					u.mender.CleanupDeltaFile(downloads[j].deltaPath)
				}
			}
			u.fallbackToFullUpdate(releases, variantID, fmt.Sprintf("apply failed: %v", err))
			return
		}

		u.logger.Printf("Delta %d/%d applied successfully: %s", deltaNum, len(downloads), newMenderPath)

		// Clear progress
		if err := u.status.ClearInstallProgress(u.ctx); err != nil {
			u.logger.Printf("Failed to clear install progress: %v", err)
		}

		workingVersion = targetVersion
		finalMenderPath = newMenderPath
	}

	u.logger.Printf("All %d deltas applied successfully", len(downloads))

	// Step 4: Check for additional deltas that may have been released during the update
	u.logger.Printf("Checking for additional deltas released during update...")
	freshReleases, err := u.githubAPI.GetReleases()
	if err != nil {
		u.logger.Printf("Warning: Failed to check for new releases: %v (proceeding with install)", err)
	} else {
		additionalChain, err := u.buildDeltaChain(freshReleases, workingVersion, u.config.Channel, variantID)
		if err == nil && len(additionalChain) > 0 {
			u.logger.Printf("Found %d additional deltas released during update!", len(additionalChain))

			// Update target version
			newLatestVersion := strings.ToLower(additionalChain[len(additionalChain)-1].TagName)
			if err := u.status.SetStatusAndVersion(u.ctx, status.StatusDownloading, newLatestVersion); err != nil {
				u.logger.Printf("Failed to update target version: %v", err)
			}

			// Download and apply additional deltas
			for i, release := range additionalChain {
				deltaURL := ""
				for _, asset := range release.Assets {
					if strings.Contains(asset.Name, variantID) && strings.HasSuffix(asset.Name, ".delta") {
						deltaURL = asset.BrowserDownloadURL
						break
					}
				}

				if deltaURL == "" {
					u.logger.Printf("No delta asset for additional release %s, stopping here", release.TagName)
					break
				}

				targetVersion := strings.ToLower(release.TagName)
				u.logger.Printf("=== Applying additional delta %d/%d: %s -> %s ===", i+1, len(additionalChain), workingVersion, targetVersion)

				// Download
				deltaPath, err := u.mender.DownloadDelta(u.ctx, deltaURL, downloadProgressCallback)
				if err != nil {
					u.logger.Printf("Failed to download additional delta: %v (proceeding with install)", err)
					break
				}

				if err := u.status.ClearDownloadProgress(u.ctx); err != nil {
					u.logger.Printf("Failed to clear download progress: %v", err)
				}

				// Apply
				newMenderPath, err := u.mender.ApplyDownloadedDelta(u.ctx, deltaPath, workingVersion, installProgressCallback)
				if err != nil {
					u.logger.Printf("Failed to apply additional delta: %v (proceeding with install)", err)
					u.mender.CleanupDeltaFile(deltaPath)
					break
				}

				if err := u.status.ClearInstallProgress(u.ctx); err != nil {
					u.logger.Printf("Failed to clear install progress: %v", err)
				}

				workingVersion = targetVersion
				finalMenderPath = newMenderPath
				u.logger.Printf("Additional delta applied: now at %s", workingVersion)
			}
		} else {
			u.logger.Printf("No additional deltas found")
		}
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

		// Check if this is a corruption error (gzip decompression, checksum failure, etc.)
		errStr := err.Error()
		isCorruptionError := strings.Contains(errStr, "gzip") ||
			strings.Contains(errStr, "checksum") ||
			strings.Contains(errStr, "corrupt") ||
			strings.Contains(errStr, "truncated")

		if isCorruptionError {
			u.logger.Printf("Installation failed due to file corruption, deleting corrupted file: %s", finalMenderPath)
			if removeErr := u.mender.RemoveFile(finalMenderPath); removeErr != nil {
				u.logger.Printf("Warning: Failed to delete corrupted file: %v", removeErr)
			} else {
				u.logger.Printf("Deleted corrupted file, next update check will re-download")
			}
		}

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

// fallbackToFullUpdate clears delta error state and falls back to a full update
func (u *Updater) fallbackToFullUpdate(releases []Release, variantID, reason string) {
	u.logger.Printf("Delta update failed (%s), would fall back to full update (DISABLED FOR TESTING)", reason)

	if err := u.status.SetError(u.ctx, "delta-failed", reason); err != nil {
		u.logger.Printf("Failed to set error status: %v", err)
	}

	// TODO: Re-enable fallback after testing
	return

	time.Sleep(2 * time.Second)
	if err := u.status.SetIdleAndClearVersion(u.ctx); err != nil {
		u.logger.Printf("Failed to clear status before fallback: %v", err)
	}

	latestRelease, found := u.findLatestRelease(releases, variantID, u.config.Channel)
	if found {
		menderURL := u.findMenderAsset(latestRelease, variantID)
		if menderURL != "" {
			u.logger.Printf("Starting full update to %s", latestRelease.TagName)
			u.performUpdate(latestRelease, menderURL)
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
	u.logger.Printf("Monitoring vehicle state for 'stand-by' using HashWatcher")

	// Create a channel to signal when standby duration is met
	standbyMet := make(chan error, 1)
	var once sync.Once

	// Use HashWatcher to monitor vehicle state changes
	watcher := u.redis.NewVehicleWatcher(config.VehicleHashKey)
	watcher.OnField("state", func(state string) error {
		currentState, _, err := u.redis.GetVehicleStateWithTimestamp(config.VehicleHashKey)
		if err != nil {
			u.logger.Printf("Failed to get vehicle state after change: %v", err)
			return nil
		}

		if currentState == "stand-by" {
			// Use ota:standby-timer-start as the authoritative source (set by vehicle-service on standby entry)
			standbyTimerStart, err := u.redis.GetStandbyTimerStart()
			if err == nil && !standbyTimerStart.IsZero() {
				u.standbyStartTime = standbyTimerStart
			} else {
				u.standbyStartTime = time.Now()
			}
			u.logger.Printf("Vehicle entered 'stand-by' state at %s. Monitoring for %v.", u.standbyStartTime.Format(time.RFC3339), requiredDuration)

			// Check if already waited long enough
			durationInStandby := time.Since(u.standbyStartTime)
			if durationInStandby >= requiredDuration {
				u.logger.Printf("Vehicle has been in 'stand-by' for %v. Proceeding with MDB reboot.", durationInStandby)
				u.logger.Printf("Triggering MDB reboot via Redis command")
				once.Do(func() { standbyMet <- u.redis.TriggerReboot() })
			} else {
				// Start precise timer for remaining duration
				remainingTime := requiredDuration - durationInStandby
				u.logger.Printf("Vehicle in 'stand-by' for %v. Starting precise timer for %v.", durationInStandby, remainingTime)
				once.Do(func() { standbyMet <- u.waitForStandbyTimer(remainingTime) })
			}
		} else {
			if !u.standbyStartTime.IsZero() {
				u.logger.Printf("Vehicle left 'stand-by' state (current: %s). Resetting standby timer.", currentState)
				u.standbyStartTime = time.Time{}
			} else {
				u.logger.Printf("Vehicle not in 'stand-by' (current: %s). Waiting.", currentState)
			}
		}
		return nil
	})

	if err := watcher.Start(); err != nil {
		u.logger.Printf("Failed to start vehicle watcher: %v. Falling back to polling.", err)
		return u.waitForStandbyWithPolling(requiredDuration)
	}
	defer watcher.Stop()

	// Wait for either context cancellation or standby duration to be met
	select {
	case <-u.ctx.Done():
		u.logger.Printf("MDB reboot cancelled due to context done.")
		return u.ctx.Err()
	case err := <-standbyMet:
		return err
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

	if len(deltaChain) == 0 {
		if !foundCurrent {
			return nil, fmt.Errorf("current version %s not found in %s releases", currentVersion, channel)
		}
		return nil, nil
	}

	return deltaChain, nil
}
