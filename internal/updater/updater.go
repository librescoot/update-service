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

	"github.com/librescoot/update-service/internal/boot"
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
	bootUpdater      *boot.BootUpdater // nil if --boot-update not set
	bootStatus       *status.Reporter  // reporter for "{component}-boot" keys
	dbcStatus        *status.Reporter  // reporter for "dbc" keys (MDB-only, for clearing stale DBC state)
	githubAPI        *GitHubAPI
	logger           *log.Logger
	ctx              context.Context
	cancel           context.CancelFunc
	standbyMu        sync.RWMutex
	standbyStartTime time.Time // Tracks when vehicle entered standby state

	// Update method configuration
	updateMethodMu sync.RWMutex
	updateMethod   string

	// Prevent concurrent update checks
	updateCheckMu sync.Mutex

	// Serializes every long-running update operation (file install, URL install,
	// full, delta). Held for the full duration of the operation — startup's
	// pending-file install used to race with a concurrent `check-now` delta, and
	// the file install's `complete-dbc` would cut dashboard power mid-delta.
	updateOpMu sync.Mutex

	// DBC orchestration (MDB-only)
	orchestrateDBCMu      sync.RWMutex
	orchestrateDBCEnabled bool
	dbcOrchestrating      sync.Mutex // prevents overlapping orchestration

	// Wakes updateCheckLoop to reload config.CheckInterval and reset its timer
	// when the setting changes at runtime (e.g., set to 0 to disable checks).
	checkIntervalChanged chan struct{}

	// Track active update goroutines for clean shutdown
	wg sync.WaitGroup
}

// menderInstallProgressCb returns a callback that reports mender-update install
// progress (parsed from stderr) to the status reporter as 0-100%.
// Used for full (non-delta) updates where mender install is the entire install phase.
func (u *Updater) menderInstallProgressCb() mender.InstallProgressCallback {
	return func(percent int) {
		if err := u.status.SetInstallProgress(u.ctx, percent); err != nil {
			u.logger.Printf("Failed to set install progress: %v", err)
		}
	}
}

// New creates a new component-aware updater
func New(ctx context.Context, cfg *config.Config, redisClient *redis.Client, inhibitorClient *inhibitor.Client, powerClient *power.Client, bootUpdater *boot.BootUpdater, logger *log.Logger) *Updater {
	updaterCtx, cancel := context.WithCancel(ctx)

	// Determine download directory
	downloadDir := cfg.DownloadDir
	if downloadDir == "" {
		downloadDir = filepath.Join("/data/ota", cfg.Component)
	}

	var bootStatusReporter *status.Reporter
	if bootUpdater != nil {
		bootStatusReporter = status.NewReporter(redisClient.GetClient(), cfg.Component+"-boot", logger)
	}

	var dbcStatusReporter *status.Reporter
	if cfg.Component == "mdb" {
		dbcStatusReporter = status.NewReporter(redisClient.GetClient(), "dbc", logger)
	}

	u := &Updater{
		config:               cfg,
		redis:                redisClient,
		inhibitor:            inhibitorClient,
		power:                powerClient,
		mender:               mender.NewManager(downloadDir, logger),
		status:               status.NewReporter(redisClient.GetClient(), cfg.Component, logger),
		bootUpdater:          bootUpdater,
		bootStatus:           bootStatusReporter,
		dbcStatus:            dbcStatusReporter,
		githubAPI:            NewGitHubAPI(updaterCtx, cfg.ReleasesURL, logger),
		logger:               logger,
		ctx:                  updaterCtx,
		cancel:               cancel,
		checkIntervalChanged: make(chan struct{}, 1),
	}

	// Initialize update method from Redis
	updateMethod, err := redisClient.GetUpdateMethod(cfg.Component)
	if err != nil {
		logger.Printf("Failed to get initial update method for %s: %v (defaulting to delta)", cfg.Component, err)
		updateMethod = "delta"
	}
	u.updateMethod = updateMethod

	// Initialize DBC orchestration setting (MDB-only)
	if cfg.Component == "mdb" {
		u.orchestrateDBCEnabled = u.resolveOrchestrateDBC()
		logger.Printf("DBC orchestration: %v", u.orchestrateDBCEnabled)
	}

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
		u.logger.Printf("Mender in inconsistent state, attempting rollback to clean up")
		if err := u.mender.Rollback(); err != nil {
			u.logger.Printf("Rollback failed: %v (may need manual intervention)", err)
		}
		return false, nil

	default:
		return false, nil
	}
}

// Start starts the updater. The menderNeedsReboot parameter indicates if
// CheckAndCommitPendingUpdate detected that mender has an update waiting for reboot.
func (u *Updater) Start(menderNeedsReboot bool) error {
	// Clean up stale temp files from previous runs (killed mid-update etc.)
	if err := u.cleanupDeltaTempDirs(); err != nil {
		u.logger.Printf("Warning: Failed to cleanup stale temp dirs: %v", err)
	}

	// Remove old .mender files, keeping only the newest one
	u.mender.CleanupStaleMenderFiles()

	// Recover from any stuck status on startup
	if err := u.recoverFromStuckState(menderNeedsReboot); err != nil {
		u.logger.Printf("Warning: Failed to recover from stuck state: %v", err)
	}

	// Publish installed boot version to Redis on startup
	if u.bootUpdater != nil && u.bootStatus != nil {
		if bootVer, err := u.bootUpdater.GetInstalledVersion(); err != nil {
			u.logger.Printf("Warning: Failed to read boot version: %v", err)
		} else if bootVer != "" {
			if err := u.bootStatus.SetUpdateVersion(u.ctx, bootVer); err != nil {
				u.logger.Printf("Warning: Failed to publish boot version to Redis: %v", err)
			} else {
				u.logger.Printf("Boot version: %s", bootVer)
			}
		}
	}

	// Check if local boot assets need to be applied (runs once, synchronously)
	u.performLocalBootUpdate()

	// Initialize Redis keys if they don't exist
	if err := u.initializeRedisKeys(); err != nil {
		u.logger.Printf("Warning: Failed to initialize Redis keys: %v", err)
	}

	// Check if we have a mender file newer than the running version (e.g., from
	// an interrupted update). If so, install it directly without re-downloading.
	u.installPendingMenderFile(menderNeedsReboot)

	// Check initial vehicle state and set standby timestamp if needed
	u.checkInitialStandbyState()

	// Keep standby timer in sync with live vehicle state changes
	go u.monitorVehicleState()

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
	u.wg.Wait()
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

// installPendingMenderFile checks if a mender file newer than the running
// version exists on disk (e.g., from an interrupted update) and installs it.
// When menderNeedsReboot is true, mender already has a staged update in its
// LMDB — attempting another install would fail with "already in progress".
func (u *Updater) installPendingMenderFile(menderNeedsReboot bool) {
	if menderNeedsReboot {
		u.logger.Printf("Skipping pending mender file check (mender has an active update)")
		return
	}

	currentVersion, err := u.getCurrentVersion()
	if err != nil || currentVersion == "" {
		return
	}

	menderPath, menderVersion, found := u.mender.FindLatestMenderFile(u.config.Channel)
	if !found || menderVersion == "" {
		return
	}

	// Compare: if the mender file version matches the running version, nothing to do
	if strings.ToLower(menderVersion) == strings.ToLower(currentVersion) {
		return
	}

	// For nightly/testing, versions are timestamps — newer = lexicographically greater
	if strings.ToLower(menderVersion) <= strings.ToLower(currentVersion) {
		return
	}

	u.logger.Printf("Found pending mender file %s (running %s), installing", menderVersion, currentVersion)

	// Publish non-idle status synchronously before spawning the goroutine —
	// otherwise a command like `check-now` arriving in the same millisecond can
	// see StatusIdle, skip the guard in checkForUpdates, and spawn a concurrent
	// delta update that races the file install.
	if err := u.status.SetDownloading(u.ctx, strings.ToLower(menderVersion), "full"); err != nil {
		u.logger.Printf("Failed to set downloading status before pending install: %v", err)
	}

	// Install in background so we don't block startup
	u.wg.Add(1)
	go func() {
		defer u.wg.Done()
		u.handleUpdateFromFile(menderPath)
	}()
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
	case status.StatusDownloading, status.StatusPreparing:
		// Check if file exists for logging purposes
		targetVersion, _ := u.redis.GetTargetVersion(u.config.Component)
		if targetVersion != "" {
			if filePath, exists := u.mender.FindMenderFileForVersion(targetVersion); exists {
				u.logger.Printf("Found complete file %s, next check will resume install", filePath)
			}
		}

		u.logger.Printf("Clearing %s status", currentStatus)
		if err := u.status.SetIdle(u.ctx); err != nil {
			return fmt.Errorf("failed to clear %s status: %w", currentStatus, err)
		}

	case status.StatusInstalling:
		if menderNeedsReboot {
			// Mender install completed but needs reboot
			u.logger.Printf("Mender install complete, setting status to pending-reboot")
			if err := u.status.SetPendingReboot(u.ctx); err != nil {
				return fmt.Errorf("failed to set pending-reboot status: %w", err)
			}
		} else {
			// Mender install failed or was interrupted
			u.logger.Printf("Clearing installing status (mender has no pending update)")
			if err := u.status.SetIdle(u.ctx); err != nil {
				return fmt.Errorf("failed to clear installing status: %w", err)
			}
		}

	case status.StatusPendingReboot:
		if menderNeedsReboot {
			// Mender still has a staged update waiting for reboot — keep status
			u.logger.Printf("Keeping pending-reboot status (mender still needs reboot)")
		} else {
			// Reboot happened, commit was already attempted by CheckAndCommitPendingUpdate
			u.logger.Printf("Clearing pending-reboot status (reboot completed)")
			if err := u.status.SetIdle(u.ctx); err != nil {
				return fmt.Errorf("failed to clear pending-reboot status: %w", err)
			}

			// For DBC component, notify vehicle-service that update is complete
			// (the defer that normally sends this didn't execute due to reboot)
			if u.config.Component == "dbc" {
				u.logger.Printf("Sending complete-dbc command after reboot")
				if err := u.redis.PushUpdateCommand("complete-dbc"); err != nil {
					u.logger.Printf("Failed to send complete-dbc command: %v", err)
				}
			}
		}

	case status.StatusError:
		u.logger.Printf("Clearing error status to allow retry")
		if err := u.status.SetIdle(u.ctx); err != nil {
			return fmt.Errorf("failed to clear error status: %w", err)
		}
	}

	// Recover boot status independently
	if u.bootStatus != nil {
		if err := u.recoverBootStatus(); err != nil {
			u.logger.Printf("Warning: Failed to recover boot status: %v", err)
		}
	}

	return nil
}

// recoverBootStatus resets a stuck boot update status on startup.
// If boot status is "pending-reboot" on startup, the reboot already happened, clear to idle.
func (u *Updater) recoverBootStatus() error {
	bootComp := bootComponent(u.config.Component)
	currentStatus, err := u.bootStatus.GetStatus(u.ctx)
	if err != nil {
		return fmt.Errorf("failed to get boot status: %w", err)
	}

	if currentStatus == status.StatusIdle || currentStatus == "" {
		return nil
	}

	u.logger.Printf("Found %s status for %s-boot on startup", currentStatus, u.config.Component)

	switch currentStatus {
	case status.StatusPendingReboot:
		u.logger.Printf("Clearing %s-boot pending-reboot status (reboot completed)", u.config.Component)
		if u.config.Component == "dbc" {
			u.logger.Printf("Sending complete-dbc command after boot reboot")
			if err := u.redis.PushUpdateCommand("complete-dbc"); err != nil {
				u.logger.Printf("Failed to send complete-dbc command: %v", err)
			}
		}
		if err := u.bootStatus.SetIdle(u.ctx); err != nil {
			return fmt.Errorf("failed to clear pending-reboot status: %w", err)
		}
	case status.StatusDownloading, status.StatusPreparing, status.StatusInstalling, status.StatusError:
		u.logger.Printf("Clearing %s-boot %s status", bootComp, currentStatus)
		if err := u.bootStatus.SetIdle(u.ctx); err != nil {
			return fmt.Errorf("failed to clear %s status: %w", currentStatus, err)
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

func (u *Updater) getStandbyStartTime() time.Time {
	u.standbyMu.RLock()
	defer u.standbyMu.RUnlock()
	return u.standbyStartTime
}

func (u *Updater) setStandbyStartTime(t time.Time) {
	u.standbyMu.Lock()
	defer u.standbyMu.Unlock()
	u.standbyStartTime = t
}

// monitorVehicleState keeps standbyStartTime in sync with the actual vehicle state.
// This runs for the lifetime of the service so TriggerReboot always has current data.
func (u *Updater) monitorVehicleState() {
	watcher := u.redis.NewVehicleWatcher(config.VehicleHashKey)

	// MDB-only: when DBC is powered off while its OTA status is stuck in
	// "pending-reboot" (e.g. DBC finished installing but got shut down before
	// rebooting), clear the stale state so a future orchestration cycle will
	// power the DBC back on instead of treating it as "already updating".
	if u.dbcStatus != nil {
		watcher.OnField("dashboard:power", func(power string) error {
			if power != "off" {
				return nil
			}
			dbcOTA, err := u.redis.GetDBCOTAStatus()
			if err != nil || dbcOTA != string(status.StatusPendingReboot) {
				return nil
			}
			u.logger.Printf("[dbc-orchestrate] DBC powered off while status=pending-reboot; clearing stale state")
			if err := u.dbcStatus.SetIdle(u.ctx); err != nil {
				u.logger.Printf("[dbc-orchestrate] Failed to clear DBC pending-reboot state: %v", err)
			}
			return nil
		})
	}

	watcher.OnField("state", func(state string) error {
		if state == "stand-by" {
			if u.getStandbyStartTime().IsZero() {
				_, stateTs, err := u.redis.GetVehicleStateWithTimestamp(config.VehicleHashKey)
				if err == nil && !stateTs.IsZero() {
					u.setStandbyStartTime(stateTs)
					u.logger.Printf("Vehicle entered 'stand-by' at %s", stateTs.Format(time.RFC3339))
				} else {
					u.setStandbyStartTime(time.Now())
					u.logger.Printf("Vehicle entered 'stand-by' (no timestamp, using current time)")
				}
			}
		} else {
			prev := u.getStandbyStartTime()
			if !prev.IsZero() {
				u.logger.Printf("Vehicle left 'stand-by' (now: %s). Clearing standby timer.", state)
				u.setStandbyStartTime(time.Time{})
			}
		}
		return nil
	})

	if err := watcher.StartWithSync(); err != nil {
		u.logger.Printf("Failed to start vehicle state watcher: %v (standby tracking may be stale)", err)
		return
	}

	// Block until context is cancelled
	<-u.ctx.Done()
	watcher.Stop()
}

// checkInitialStandbyState checks the initial vehicle state on startup and sets standby timestamp
func (u *Updater) checkInitialStandbyState() {
	currentState, stateTs, err := u.redis.GetVehicleStateWithTimestamp(config.VehicleHashKey)
	if err != nil {
		u.logger.Printf("Vehicle state: unknown (%v)", err)
		return
	}

	if currentState == "stand-by" {
		if !stateTs.IsZero() {
			u.setStandbyStartTime(stateTs)
			u.logger.Printf("Vehicle in 'stand-by' since %s (%v ago)", stateTs.Format(time.RFC3339), time.Since(stateTs))
		} else {
			u.setStandbyStartTime(time.Now())
			u.logger.Printf("Vehicle in 'stand-by' (no timestamp, using current time)")
		}
	} else {
		u.logger.Printf("Vehicle state: %s (will wait for stand-by before reboot)", currentState)
	}
}

// revalidateStandbyState re-validates the vehicle state after long-running operations
// If vehicle stayed in standby, state:timestamp will be unchanged and we preserve the original time.
// If vehicle left and re-entered standby, state:timestamp will reflect the new entry time.
func (u *Updater) revalidateStandbyState() {
	currentState, stateTs, err := u.redis.GetVehicleStateWithTimestamp(config.VehicleHashKey)
	if err != nil {
		u.logger.Printf("Failed to get vehicle state after long-running operation: %v (clearing standby timestamp)", err)
		u.setStandbyStartTime(time.Time{})
		return
	}

	if currentState == "stand-by" {
		if !stateTs.IsZero() {
			prev := u.getStandbyStartTime()
			if stateTs.Equal(prev) {
				u.logger.Printf("Vehicle still in 'stand-by' since %s (%v elapsed)", stateTs.Format(time.RFC3339), time.Since(stateTs))
			} else {
				u.setStandbyStartTime(stateTs)
				u.logger.Printf("Vehicle re-entered 'stand-by' at %s (%v ago)", stateTs.Format(time.RFC3339), time.Since(stateTs))
			}
		} else {
			u.setStandbyStartTime(time.Now())
			u.logger.Printf("Vehicle in 'stand-by' after operation (no timestamp) - using current time")
		}
	} else {
		u.logger.Printf("Vehicle not in 'stand-by' after operation (current: %s) - clearing standby timestamp", currentState)
		u.setStandbyStartTime(time.Time{})
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
		u.logger.Printf("Manual update check triggered")
		u.wg.Add(1)
		go func() {
			defer u.wg.Done()
			u.checkForUpdates()
		}()

	case strings.HasPrefix(command, "update-from-file:"):
		filePath := strings.TrimSpace(strings.TrimPrefix(command, "update-from-file:"))
		if filePath == "" {
			u.logger.Printf("Received invalid update-from-file command with empty path")
			return
		}
		u.logger.Printf("Received update-from-file command: %s", filePath)
		u.wg.Add(1)
		go func() {
			defer u.wg.Done()
			u.handleUpdateFromFile(filePath)
		}()

	case strings.HasPrefix(command, "update-from-url:"):
		url := strings.TrimPrefix(command, "update-from-url:")
		u.logger.Printf("Received update-from-url command: %s", url)
		u.wg.Add(1)
		go func() {
			defer u.wg.Done()
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

	if !u.updateOpMu.TryLock() {
		u.logger.Printf("Update already in progress, ignoring file request: %s", source)
		return
	}
	defer u.updateOpMu.Unlock()

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

	if err := u.status.SetDownloading(u.ctx, version, "full"); err != nil {
		u.logger.Printf("Failed to set downloading status: %v", err)
		return
	}

	if err := u.status.SetInstalling(u.ctx); err != nil {
		u.logger.Printf("Failed to set installing status: %v", err)
		return
	}

	if u.config.Component == "mdb" {
		if err := u.inhibitor.AddInstallInhibit(u.config.Component); err != nil {
			u.logger.Printf("Failed to add install inhibit: %v", err)
		}
		defer func() {
			if err := u.inhibitor.RemoveInstallInhibit(u.config.Component); err != nil {
				u.logger.Printf("Failed to remove install inhibit: %v", err)
			}
		}()
	}

	if u.config.Component == "dbc" {
		u.logger.Printf("Starting DBC file update - sending start-dbc command")
		if err := u.redis.PushUpdateCommand("start-dbc"); err != nil {
			u.logger.Printf("Failed to send start-dbc command: %v", err)
		}
		defer func() {
			u.logger.Printf("DBC file update cleanup - sending complete-dbc command")
			if err := u.redis.PushUpdateCommand("complete-dbc"); err != nil {
				u.logger.Printf("Failed to send complete-dbc command: %v", err)
			}
		}()
	}

	if err := u.mender.Install(source, u.menderInstallProgressCb()); err != nil {
		u.logger.Printf("Failed to install update from file %s: %v", source, err)

		errStr := err.Error()
		isCorruptionError := strings.Contains(errStr, "gzip") ||
			strings.Contains(errStr, "checksum") ||
			strings.Contains(errStr, "corrupt") ||
			strings.Contains(errStr, "truncated")

		if isCorruptionError {
			u.logger.Printf("Installation failed due to file corruption, deleting corrupted file: %s", source)
			if removeErr := u.mender.RemoveFile(source); removeErr != nil {
				u.logger.Printf("Warning: Failed to delete corrupted file: %v", removeErr)
			} else {
				u.logger.Printf("Deleted corrupted file, restarting update check")
				u.restartCheckAfterCorruptFile()
				return
			}
		}

		if err := u.status.SetError(u.ctx, "install-failed", fmt.Sprintf("Failed to install update from file %s: %v", source, err)); err != nil {
			u.logger.Printf("Failed to set error status: %v", err)
		}
		return
	}

	u.logger.Printf("Successfully installed update from file: %s", source)

	if err := u.status.SetPendingReboot(u.ctx); err != nil {
		u.logger.Printf("Failed to set pending-reboot status: %v", err)
	}

	if err := u.TriggerReboot(u.config.Component); err != nil {
		u.logger.Printf("Failed to trigger %s reboot after file update: %v", u.config.Component, err)
		if !strings.Contains(err.Error(), "DRY-RUN") {
			if statusErr := u.status.SetError(u.ctx, "reboot-failed", fmt.Sprintf("Failed to trigger %s reboot: %v", u.config.Component, err)); statusErr != nil {
				u.logger.Printf("Additionally failed to set error status after reboot trigger failure: %v", statusErr)
			}
			return
		}

		u.logger.Printf("Dry run: setting idle status for %s after file update", u.config.Component)
		if idleErr := u.status.SetIdle(u.ctx); idleErr != nil {
			u.logger.Printf("Failed to set idle status in dry run for %s: %v", u.config.Component, idleErr)
		}
	}
}

// handleUpdateFromURL processes an update from a URL
// Supports format: https://example.com/file.mender or https://example.com/file.mender:sha256:checksum
func (u *Updater) handleUpdateFromURL(url string) {
	source, checksum, _ := u.parseUpdateSource(url)

	if !u.updateOpMu.TryLock() {
		u.logger.Printf("Update already in progress, ignoring URL request: %s", source)
		return
	}
	defer u.updateOpMu.Unlock()

	if checksum != "" {
		u.logger.Printf("Checksum provided: %s", checksum)
	}

	// For DBC updates, notify vehicle-service to keep dashboard power on
	if u.config.Component == "dbc" {
		u.logger.Printf("Starting DBC URL update - sending start-dbc command")
		if err := u.redis.PushUpdateCommand("start-dbc"); err != nil {
			u.logger.Printf("Failed to send start-dbc command: %v", err)
		}
		defer func() {
			u.logger.Printf("DBC URL update finished - sending complete-dbc command")
			if err := u.redis.PushUpdateCommand("complete-dbc"); err != nil {
				u.logger.Printf("Failed to send complete-dbc command: %v", err)
			}
		}()
	}

	// Add download inhibit (MDB only, vehicle-service handles DBC power)
	if u.config.Component == "mdb" {
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
		}()
	}

	u.logger.Printf("Processing update from URL: %s", source)

	updateMethod := u.getUpdateMethod()
	if err := u.status.SetDownloading(u.ctx, "unknown", updateMethod); err != nil {
		u.logger.Printf("Failed to set downloading status: %v", err)
		return
	}

	for i := range 5 {
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

		u.revalidateStandbyState()

		if err := u.status.SetInstalling(u.ctx); err != nil {
			u.logger.Printf("Failed to set installing status: %v", err)
			return
		}

		if u.config.Component == "mdb" {
			if err := u.inhibitor.AddInstallInhibit(u.config.Component); err != nil {
				u.logger.Printf("Failed to add install inhibit: %v", err)
			}
			if err := u.inhibitor.RemoveDownloadInhibit(u.config.Component); err != nil {
				u.logger.Printf("Failed to remove download inhibit: %v", err)
			}
		}

		if err := u.mender.Install(filePath, u.menderInstallProgressCb()); err != nil {
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
					u.logger.Printf("Deleted corrupted file, restarting update check")
					u.restartCheckAfterCorruptFile()
					return
				}
			}

			if err := u.status.SetError(u.ctx, "install-failed", fmt.Sprintf("Failed to install update: %v", err)); err != nil {
				u.logger.Printf("Failed to set error status: %v", err)
			}
			return
		}

		u.logger.Printf("Successfully installed update")

		if err := u.status.SetPendingReboot(u.ctx); err != nil {
			u.logger.Printf("Failed to set pending-reboot status: %v", err)
		}

		if u.config.Component == "mdb" {
			if err := u.inhibitor.RemoveInstallInhibit(u.config.Component); err != nil {
				u.logger.Printf("Failed to remove install inhibit: %v", err)
			}
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
					if idleErr := u.status.SetIdle(u.ctx); idleErr != nil {
						u.logger.Printf("Failed to set idle status in dry run for %s: %v", u.config.Component, idleErr)
					}
				}
			}
		} else {
			u.logger.Printf("Unknown component %s, cannot determine reboot strategy. Setting to idle.", u.config.Component)
			if err := u.status.SetIdle(u.ctx); err != nil {
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

	if u.config.Component == "mdb" {
		watcher.OnField("updates.mdb.orchestrate-dbc", func(value string) error {
			u.logger.Printf("Received settings change for updates.mdb.orchestrate-dbc: %s", value)
			switch value {
			case "true", "false":
				u.setOrchestrateDBC(value == "true")
			default:
				// Setting cleared — fall back to channel-dependent default
				u.setOrchestrateDBC(u.resolveOrchestrateDBC())
			}
			return nil
		})
	}

	if err := watcher.Start(); err != nil {
		u.logger.Printf("Failed to start settings watcher: %v", err)
		return
	}
	defer watcher.Stop()

	// Wait for context cancellation
	<-u.ctx.Done()
	u.logger.Printf("Settings monitor stopped for %s", u.config.Component)
}

// updateCheckLoop periodically checks for updates. Reacts to runtime changes
// of config.CheckInterval via NotifyCheckIntervalChanged so that setting the
// interval to 0 actually stops the loop and changing it resets the timer.
func (u *Updater) updateCheckLoop() {
	// Initial check on startup if enabled
	if u.config.CheckInterval > 0 {
		u.checkForUpdates()
	}

	for {
		interval := u.config.CheckInterval

		if interval <= 0 {
			// Disabled — block until the interval is re-enabled or we shut down
			select {
			case <-u.ctx.Done():
				u.logger.Printf("Update check loop stopped")
				return
			case <-u.checkIntervalChanged:
				continue
			}
		}

		timer := time.NewTimer(interval)
		select {
		case <-u.ctx.Done():
			timer.Stop()
			u.logger.Printf("Update check loop stopped")
			return
		case <-u.checkIntervalChanged:
			if !timer.Stop() {
				<-timer.C
			}
			continue
		case <-timer.C:
			// Re-read in case the config was set to 0 just before the timer fired.
			if u.config.CheckInterval > 0 {
				u.checkForUpdates()
			}
		}
	}
}

// restartCheckAfterCorruptFile clears the current status to idle after a
// corrupted file was deleted, so the error status doesn't block further
// checks. If periodic checks are enabled, also kicks off an immediate check
// in a new goroutine so a fresh download can be attempted right away.
// With CheckInterval=0 (auto-checks disabled), we don't self-trigger and
// wait for the next manual trigger.
func (u *Updater) restartCheckAfterCorruptFile() {
	if err := u.status.SetIdle(u.ctx); err != nil {
		u.logger.Printf("Failed to clear status after corrupt file: %v", err)
		return
	}
	if u.config.CheckInterval == 0 {
		return
	}
	u.wg.Add(1)
	go func() {
		defer u.wg.Done()
		u.checkForUpdates()
	}()
}

// NotifyCheckIntervalChanged signals updateCheckLoop to reload the configured
// check interval and reset its timer. Call this after mutating
// config.CheckInterval at runtime. Non-blocking; coalesces pending signals.
func (u *Updater) NotifyCheckIntervalChanged() {
	select {
	case u.checkIntervalChanged <- struct{}{}:
	default:
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
	releases, err := u.githubAPI.GetReleases(u.config.Channel)
	if err != nil {
		u.logger.Printf("Failed to get releases: %v", err)
		return
	}

	// If running on MDB, check if DBC needs an update too (runs in parallel with MDB's own update)
	if u.config.Component == "mdb" {
		u.wg.Add(1)
		go func() {
			defer u.wg.Done()
			u.orchestrateDBC(releases)
		}()
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
		// Attempt delta update for rootfs
		u.wg.Add(1)
		go func() {
			defer u.wg.Done()
			u.performDeltaUpdate(releases, currentVersion, variantID, false)
		}()

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
			assetURL = asset.URL
			u.logger.Printf("Found matching asset: %s", asset.Name)
			break
		}
	}

	if assetURL == "" {
		u.logger.Printf("No .mender asset found for variant_id %s in release %s", variantID, release.TagName)
	} else if !u.isUpdateNeeded(release) {
		u.logger.Printf("No update needed for component %s", u.config.Component)
	} else {
		u.logger.Printf("Update needed for %s: %s (using full update)", u.config.Component, release.TagName)
		u.wg.Add(1)
		go func() {
			defer u.wg.Done()
			u.performUpdate(release, assetURL)
		}()
	}

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

	maxLen := max(len(parts2), len(parts1))

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

// performUpdate is the public entry point for a full update — acquires
// updateOpMu so it won't race with another in-flight update operation.
func (u *Updater) performUpdate(release Release, assetURL string) {
	if !u.updateOpMu.TryLock() {
		u.logger.Printf("Update already in progress, skipping full update to %s", release.TagName)
		return
	}
	defer u.updateOpMu.Unlock()
	u.performUpdateLocked(release, assetURL)
}

// performUpdateLocked does the actual update work. Caller must already hold
// updateOpMu — internal callers inside performDeltaUpdate / fallbackToFullUpdate
// run under the same lock and use this entry directly to avoid deadlock.
func (u *Updater) performUpdateLocked(release Release, assetURL string) {
	u.logger.Printf("Starting update process for %s to version %s", u.config.Component, release.TagName)

	var version string
	if u.config.Channel == "stable" {
		version = release.TagName
	} else {
		// Use full tag name for nightly/testing too
		version = strings.ToLower(release.TagName)
	}

	// Step 1: Set downloading status, update method, and add download inhibitor
	if err := u.status.SetDownloading(u.ctx, version, "full"); err != nil {
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

	if u.config.Component == "mdb" {
		if err := u.inhibitor.AddDownloadInhibit(u.config.Component); err != nil {
			u.logger.Printf("Failed to add download inhibit: %v", err)
		}
	}

	// Request ondemand CPU governor for optimal download performance
	if err := u.power.RequestOndemandGovernor(); err != nil {
		u.logger.Printf("Failed to request ondemand governor: %v", err)
	}

	defer func() {
		if u.config.Component == "mdb" {
			if err := u.inhibitor.RemoveDownloadInhibit(u.config.Component); err != nil {
				u.logger.Printf("Failed to remove download inhibit: %v", err)
			}
			if err := u.inhibitor.RemoveInstallInhibit(u.config.Component); err != nil {
				u.logger.Printf("Failed to remove install inhibit: %v", err)
			}
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

	u.logger.Printf("Successfully downloaded update to: %s", filePath)

	// Re-validate vehicle state after long-running download operation
	// This ensures the 3-minute standby requirement starts fresh from the current state
	u.revalidateStandbyState()

	// Step 3: Set installing status and swap inhibitors
	if err := u.status.SetInstalling(u.ctx); err != nil {
		u.logger.Printf("Failed to set installing status: %v", err)
		return
	}

	if u.config.Component == "mdb" {
		if err := u.inhibitor.AddInstallInhibit(u.config.Component); err != nil {
			u.logger.Printf("Failed to add install inhibit: %v", err)
		}
		if err := u.inhibitor.RemoveDownloadInhibit(u.config.Component); err != nil {
			u.logger.Printf("Failed to remove download inhibit: %v", err)
		}
	}

	// Step 4: Install the update
	if err := u.mender.Install(filePath, u.menderInstallProgressCb()); err != nil {
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
				u.logger.Printf("Deleted corrupted file, restarting update check")
				u.restartCheckAfterCorruptFile()
				return
			}
		}

		if err := u.status.SetError(u.ctx, "install-failed", fmt.Sprintf("Failed to install update: %v", err)); err != nil {
			u.logger.Printf("Failed to set error status: %v", err)
		}
		return
	}

	u.logger.Printf("Successfully installed update")

	// Step 5: Set pending-reboot status and prepare for reboot
	if err := u.status.SetPendingReboot(u.ctx); err != nil {
		u.logger.Printf("Failed to set pending-reboot status: %v", err)
	}

	if u.config.Component == "mdb" {
		if err := u.inhibitor.RemoveInstallInhibit(u.config.Component); err != nil {
			u.logger.Printf("Failed to remove install inhibit: %v", err)
		}
	}

	// Step 6: Trigger reboot (component will reboot automatically or system will reboot)
	u.logger.Printf("Update installation complete, system will reboot to apply changes")

	// Trigger reboot
	if u.config.Component == "mdb" || u.config.Component == "dbc" {
		u.logger.Printf("%s update installed, triggering reboot", strings.ToUpper(u.config.Component))
		err := u.TriggerReboot(u.config.Component)
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
				if idleErr := u.status.SetIdle(u.ctx); idleErr != nil {
					u.logger.Printf("Failed to set idle status in dry run for %s: %v", u.config.Component, idleErr)
				}
			}
		}
		// If TriggerReboot was successful (and not a dry run), the system/component will reboot/restart.
		// Status remains 'pending-reboot'.
	} else {
		u.logger.Printf("Unknown component %s, cannot determine reboot strategy. Setting to idle.", u.config.Component)
		if err := u.status.SetIdle(u.ctx); err != nil {
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
// deltaDownloadRetryInterval is how long to wait between download attempts for a
// single delta file. Downloads are resumable, so retries cost minimal bandwidth.
const deltaDownloadRetryInterval = 5 * time.Minute

// deltaDownloadMaxRetryDuration is the maximum total time to keep retrying a
// delta download before giving up and falling back to a full update. Scooters
// may have intermittent connectivity; 15 minutes covers most transient outages.
// Can safely be raised to 24*time.Hour for very patient update behaviour.
const deltaDownloadMaxRetryDuration = 15 * time.Minute

func (u *Updater) performDeltaUpdate(releases []Release, currentVersion, variantID string, isRecheck bool) {
	// The recursive re-check call at the end of this function runs inside the
	// same goroutine that already holds updateOpMu — skip TryLock in that case
	// to avoid self-deadlocking on the non-reentrant mutex.
	if !isRecheck {
		if !u.updateOpMu.TryLock() {
			u.logger.Printf("Update already in progress, skipping delta update")
			return
		}
		defer u.updateOpMu.Unlock()
	}

	// Step 0: Determine the base version to start from
	// First, check if we have a mender file for the current running version
	baseVersion := currentVersion
	if _, exists := u.mender.FindMenderFileForVersion(currentVersion); !exists {
		// No mender file for current version - find the latest mender file we have for this channel
		_, menderVersion, found := u.mender.FindLatestMenderFile(u.config.Channel)
		if !found || menderVersion == "" {
			u.logger.Printf("No local mender file for any version on channel %s, need full update", u.config.Channel)
			u.fallbackToFullUpdate(releases, variantID, "no local mender file to base delta chain on")
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
			if !u.isUpdateNeeded(latestRelease) {
				return
			}
			menderURL := u.findMenderAsset(latestRelease, variantID)
			if menderURL != "" {
				u.logger.Printf("Falling back to full update with latest version")
				u.performUpdateLocked(latestRelease, menderURL)
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
			u.performUpdateLocked(latestRelease, menderURL)
		}
		return
	}

	if totalDeltaSize > 0 && fullUpdateSize > 0 {
		u.logger.Printf("Delta chain size: %d bytes, full update: %d bytes (saving %d bytes)",
			totalDeltaSize, fullUpdateSize, fullUpdateSize-totalDeltaSize)
	}

	// Log the delta chain
	deltaVersionList := make([]string, len(deltaChain))
	for i, release := range deltaChain {
		deltaVersionList[i] = release.TagName
	}
	u.logger.Printf("Delta chain for %s: %s -> [%s] (%d deltas, %d bytes vs %d full)",
		u.config.Component, baseVersion, strings.Join(deltaVersionList, ", "), len(deltaChain), totalDeltaSize, fullUpdateSize)

	// Set status
	if err := u.status.SetDownloading(u.ctx, latestVersion, "delta"); err != nil {
		u.logger.Printf("Failed to set downloading status: %v", err)
	}

	// For DBC updates, notify vehicle-service to keep dashboard power on
	if u.config.Component == "dbc" {
		u.logger.Printf("Starting DBC multi-delta update - sending start-dbc command")
		if err := u.redis.PushUpdateCommand("start-dbc"); err != nil {
			u.logger.Printf("Failed to send start-dbc command: %v", err)
		}
	}

	// Add power inhibit (MDB only, vehicle-service handles DBC power)
	if u.config.Component == "mdb" {
		if err := u.inhibitor.AddDownloadInhibit(u.config.Component); err != nil {
			u.logger.Printf("Failed to add download inhibit: %v", err)
		}
	}

	// Request ondemand CPU governor
	if err := u.power.RequestOndemandGovernor(); err != nil {
		u.logger.Printf("Failed to request ondemand governor: %v", err)
	}

	defer func() {
		if u.config.Component == "mdb" {
			if err := u.inhibitor.RemoveDownloadInhibit(u.config.Component); err != nil {
				u.logger.Printf("Failed to remove download inhibit: %v", err)
			}
			if err := u.inhibitor.RemovePreparingInhibit(u.config.Component); err != nil {
				u.logger.Printf("Failed to remove preparing inhibit: %v", err)
			}
			if err := u.inhibitor.RemoveInstallInhibit(u.config.Component); err != nil {
				u.logger.Printf("Failed to remove install inhibit: %v", err)
			}
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

	// Delta application progress reported as install progress
	installProgressCallback := func(percent int) {
		if err := u.status.SetInstallProgress(u.ctx, percent); err != nil {
			u.logger.Printf("Failed to set install progress: %v", err)
		}
	}

	// Step 2: Download all deltas upfront
	downloads := make([]deltaDownload, len(deltaChain))

	for i, release := range deltaChain {
		deltaURL := ""
		for _, asset := range release.Assets {
			if strings.Contains(asset.Name, variantID) && strings.HasSuffix(asset.Name, ".delta") {
				deltaURL = asset.URL
				break
			}
		}

		if deltaURL == "" {
			u.logger.Printf("No delta asset found for %s, cannot continue", release.TagName)
			for j := range i {
				if downloads[j].deltaPath != "" {
					u.mender.CleanupDeltaFile(downloads[j].deltaPath)
				}
			}
			u.fallbackToFullUpdate(releases, variantID, "no delta asset found")
			return
		}

		downloads[i].release = release
		downloads[i].deltaURL = deltaURL

		// Download with patient retry — downloads are resumable so each retry
		// picks up where the previous left off, costing minimal extra bandwidth.
		downloadDeadline := time.Now().Add(deltaDownloadMaxRetryDuration)
		var deltaPath string
		for attempt := 1; ; attempt++ {
			if attempt > 1 {
				u.logger.Printf("Downloading delta %d/%d: %s (retry %d)", i+1, len(deltaChain), release.TagName, attempt)
			}
			var err error
			deltaPath, err = u.mender.DownloadDelta(u.ctx, deltaURL, downloadProgressCallback)
			if err == nil {
				break
			}
			if time.Now().After(downloadDeadline) {
				u.logger.Printf("Download failed after %v of retries: %v", deltaDownloadMaxRetryDuration, err)
				for j := range i {
					if downloads[j].deltaPath != "" {
						u.mender.CleanupDeltaFile(downloads[j].deltaPath)
					}
				}
				u.fallbackToFullUpdate(releases, variantID, fmt.Sprintf("download failed after retries: %v", err))
				return
			}
			remaining := time.Until(downloadDeadline)
			wait := min(remaining, deltaDownloadRetryInterval)
			u.logger.Printf("Download attempt failed, retrying in %v: %v", wait, err)
			select {
			case <-u.ctx.Done():
				return
			case <-time.After(wait):
			}
		}
		downloads[i].deltaPath = deltaPath
		u.logger.Printf("Downloaded delta %d/%d: %s", i+1, len(deltaChain), release.TagName)
	}

	u.logger.Printf("All %d deltas downloaded, applying chain", len(downloads))

	// Transition to preparing: delta application is CPU-bound work
	if err := u.status.SetPreparing(u.ctx); err != nil {
		u.logger.Printf("Failed to set preparing status: %v", err)
	}

	// Swap inhibitors: preparing inhibit replaces download inhibit
	if u.config.Component == "mdb" {
		if err := u.inhibitor.AddPreparingInhibit(u.config.Component); err != nil {
			u.logger.Printf("Failed to add preparing inhibit: %v", err)
		}
		if err := u.inhibitor.RemoveDownloadInhibit(u.config.Component); err != nil {
			u.logger.Printf("Failed to remove download inhibit: %v", err)
		}
	}

	// Step 3: Apply all deltas in a single unpack/repack cycle
	deltaPaths := make([]string, len(downloads))
	deltaVersions := make([]string, len(downloads))
	for i, dl := range downloads {
		deltaPaths[i] = dl.deltaPath
		deltaVersions[i] = strings.ToLower(dl.release.TagName)
	}

	finalMenderPath, err := u.mender.ApplyDownloadedDeltaChain(u.ctx, deltaPaths, deltaVersions, baseVersion, installProgressCallback)
	if err != nil {
		if u.ctx.Err() != nil {
			u.logger.Printf("Delta chain interrupted (shutdown), will retry next run")
			return
		}
		u.logger.Printf("Delta chain apply failed: %v", err)
		u.fallbackToFullUpdate(releases, variantID, fmt.Sprintf("chain apply failed: %v", err))
		return
	}

	workingVersion := deltaVersions[len(deltaVersions)-1]
	u.logger.Printf("Delta chain applied successfully: %d deltas, now at %s", len(downloads), workingVersion)

	// Step 4: Check for additional deltas released during the update
	if u.ctx.Err() != nil {
		return
	}
	freshReleases, err := u.githubAPI.GetReleases(u.config.Channel)
	if err != nil {
		u.logger.Printf("Warning: Failed to check for new releases: %v (proceeding with install)", err)
	} else {
		additionalChain, err := u.buildDeltaChain(freshReleases, workingVersion, u.config.Channel, variantID)
		if err == nil && len(additionalChain) > 0 {
			u.logger.Printf("Found %d additional deltas released during update", len(additionalChain))

			newLatestVersion := strings.ToLower(additionalChain[len(additionalChain)-1].TagName)
			if err := u.status.SetUpdateVersion(u.ctx, newLatestVersion); err != nil {
				u.logger.Printf("Failed to update target version: %v", err)
			}

			for i, release := range additionalChain {
				if u.ctx.Err() != nil {
					u.logger.Printf("Shutdown during additional deltas, will resume next run")
					return
				}

				deltaURL := ""
				for _, asset := range release.Assets {
					if strings.Contains(asset.Name, variantID) && strings.HasSuffix(asset.Name, ".delta") {
						deltaURL = asset.URL
						break
					}
				}

				if deltaURL == "" {
					u.logger.Printf("No delta asset for additional release %s, stopping here", release.TagName)
					break
				}

				targetVersion := strings.ToLower(release.TagName)
				u.logger.Printf("Additional delta %d/%d: %s -> %s", i+1, len(additionalChain), workingVersion, targetVersion)

				deltaPath, err := u.mender.DownloadDelta(u.ctx, deltaURL, downloadProgressCallback)
				if err != nil {
					if u.ctx.Err() != nil {
						return
					}
					u.logger.Printf("Failed to download additional delta: %v (proceeding with install)", err)
					break
				}

				newMenderPath, err := u.mender.ApplyDownloadedDelta(u.ctx, deltaPath, workingVersion, installProgressCallback)
				if err != nil {
					if u.ctx.Err() != nil {
						return
					}
					u.logger.Printf("Failed to apply additional delta: %v (proceeding with install)", err)
					break
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

	// One-time re-check: fetch releases fresh to catch anything published while we were
	// applying the delta chain. If a newer version exists, restart from the just-assembled
	// version rather than installing something already stale.
	// isRecheck guards against looping if nightlies publish faster than deltas apply.
	if !isRecheck {
		recheckReleases, err := u.githubAPI.GetReleases(u.config.Channel)
		if err != nil {
			u.logger.Printf("Re-check API call failed: %v (proceeding with install)", err)
		} else if newerChain, err := u.buildDeltaChain(recheckReleases, workingVersion, u.config.Channel, variantID); err == nil && len(newerChain) > 0 {
			newerVersion := strings.ToLower(newerChain[len(newerChain)-1].TagName)
			u.logger.Printf("Newer version %s published while applying delta chain; restarting from assembled %s", newerVersion, workingVersion)
			u.performDeltaUpdate(recheckReleases, workingVersion, variantID, true)
			return
		}
	}

	// Re-validate vehicle state after long-running delta patch operation
	// This ensures the 3-minute standby requirement starts fresh from the current state
	u.revalidateStandbyState()

	// If dry-run mode, stop here - don't install or reboot
	if u.config.DryRun {
		u.logger.Printf("DRY-RUN: Multi-delta update complete. Final mender file ready at: %s", finalMenderPath)
		u.logger.Printf("DRY-RUN: Skipping mender-update install and reboot")
		if err := u.status.SetIdle(u.ctx); err != nil {
			u.logger.Printf("Failed to set idle status in dry run: %v", err)
		}
		return
	}

	// Step 4: Install the update, swap from preparing to install inhibitor
	if err := u.status.SetInstalling(u.ctx); err != nil {
		u.logger.Printf("Failed to set installing status: %v", err)
	}
	if u.config.Component == "mdb" {
		if err := u.inhibitor.AddInstallInhibit(u.config.Component); err != nil {
			u.logger.Printf("Failed to add install inhibit: %v", err)
		}
		if err := u.inhibitor.RemovePreparingInhibit(u.config.Component); err != nil {
			u.logger.Printf("Failed to remove preparing inhibit: %v", err)
		}
	}
	if err := u.mender.Install(finalMenderPath, u.menderInstallProgressCb()); err != nil {
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
				u.logger.Printf("Deleted corrupted file, restarting update check")
				u.restartCheckAfterCorruptFile()
				return
			}
		}

		if err := u.status.SetError(u.ctx, "install-failed", fmt.Sprintf("Failed to install delta-generated update: %v", err)); err != nil {
			u.logger.Printf("Failed to set error status: %v", err)
		}
		return
	}

	u.logger.Printf("Successfully installed delta update")

	// Step 5: Set pending-reboot status and prepare for reboot
	if err := u.status.SetPendingReboot(u.ctx); err != nil {
		u.logger.Printf("Failed to set pending-reboot status: %v", err)
	}

	if u.config.Component == "mdb" {
		if err := u.inhibitor.RemoveInstallInhibit(u.config.Component); err != nil {
			u.logger.Printf("Failed to remove install inhibit: %v", err)
		}
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
				if idleErr := u.status.SetIdle(u.ctx); idleErr != nil {
					u.logger.Printf("Failed to set idle status in dry run for %s: %v", u.config.Component, idleErr)
				}
			}
		}
	} else {
		u.logger.Printf("Unknown component %s, cannot determine reboot strategy. Setting to idle.", u.config.Component)
		if err := u.status.SetIdle(u.ctx); err != nil {
			u.logger.Printf("Failed to set idle status for unknown component: %v", err)
		}
	}
}

// fallbackToFullUpdate logs the delta failure reason and initiates a full update.
func (u *Updater) fallbackToFullUpdate(releases []Release, variantID, reason string) {
	u.logger.Printf("Delta update failed (%s), falling back to full update", reason)

	if err := u.status.SetError(u.ctx, "delta-failed", reason); err != nil {
		u.logger.Printf("Failed to set error status: %v", err)
	}

	time.Sleep(2 * time.Second)
	if err := u.status.SetIdle(u.ctx); err != nil {
		u.logger.Printf("Failed to clear status before fallback: %v", err)
	}

	latestRelease, found := u.findLatestRelease(releases, variantID, u.config.Channel)
	if found {
		if !u.isUpdateNeeded(latestRelease) {
			return
		}
		menderURL := u.findMenderAsset(latestRelease, variantID)
		if menderURL != "" {
			u.logger.Printf("Starting full update to %s", latestRelease.TagName)
			u.performUpdateLocked(latestRelease, menderURL)
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
		standbyStart := u.getStandbyStartTime()
		if !standbyStart.IsZero() {
			durationInStandby := time.Since(standbyStart)
			if durationInStandby >= requiredStandbyDuration {
				// Re-check current state from Redis before firing (belt-and-suspenders
				// in case the watcher missed a transition)
				currentState, _ := u.redis.GetVehicleState(config.VehicleHashKey)
				if currentState != "stand-by" {
					u.logger.Printf("Vehicle state is '%s' (standby timer stale). Waiting for stand-by.", currentState)
					u.setStandbyStartTime(time.Time{})
					return u.waitForStandbyWithSubscription(requiredStandbyDuration)
				}
				u.logger.Printf("Vehicle in 'stand-by' for %v (since %s). Proceeding with MDB reboot immediately.", durationInStandby, standbyStart.Format(time.RFC3339))
				u.logger.Printf("Triggering MDB reboot via Redis command")
				return u.redis.TriggerReboot()
			}

			// Calculate exact remaining time plus safety buffer
			remainingTime := requiredStandbyDuration - durationInStandby + safetyBuffer
			u.logger.Printf("Vehicle in 'stand-by' for %v (since %s). Sleeping for %v then rebooting.", durationInStandby, standbyStart.Format(time.RFC3339), remainingTime)

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

				totalDuration := time.Since(standbyStart)
				u.logger.Printf("Vehicle has been in 'stand-by' for %v (since %s). Proceeding with MDB reboot.", totalDuration, standbyStart.Format(time.RFC3339))
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
		// Status remains "pending-reboot" and will be cleared on next service startup
		return nil

	default:
		return fmt.Errorf("unknown component for reboot: %s", component)
	}
}

// waitForStandbyWithSubscription waits until the vehicle has been in stand-by
// for at least requiredDuration, then triggers a reboot. The permanent
// monitorVehicleState watcher keeps standbyStartTime current, so this function
// just polls that value on a short ticker.
func (u *Updater) waitForStandbyWithSubscription(requiredDuration time.Duration) error {
	u.logger.Printf("Waiting for vehicle to be in 'stand-by' for %v before rebooting", requiredDuration)

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-u.ctx.Done():
			u.logger.Printf("MDB reboot cancelled due to context done.")
			return u.ctx.Err()
		case <-ticker.C:
			standbyStart := u.getStandbyStartTime()
			if standbyStart.IsZero() {
				continue
			}

			durationInStandby := time.Since(standbyStart)
			if durationInStandby < requiredDuration {
				continue
			}

			// Final verification from Redis before rebooting
			currentState, _ := u.redis.GetVehicleState(config.VehicleHashKey)
			if currentState != "stand-by" {
				u.logger.Printf("Vehicle state is '%s' (expected stand-by). Resetting.", currentState)
				u.setStandbyStartTime(time.Time{})
				continue
			}

			u.logger.Printf("Vehicle in 'stand-by' for %v (since %s). Triggering MDB reboot.",
				durationInStandby, standbyStart.Format(time.RFC3339))
			return u.redis.TriggerReboot()
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
			return asset.URL
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
			return asset.URL
		}
	}
	return ""
}

// bootComponent returns the boot component identifier for the given rootfs component.
// e.g. "dbc" → "dbc-boot", "mdb" → "mdb-boot"
func bootComponent(component string) string {
	return component + "-boot"
}

// performLocalBootUpdate checks for boot assets baked into the rootfs and applies
// them if they differ from what's currently on the boot partition.
func (u *Updater) performLocalBootUpdate() {
	if u.bootUpdater == nil {
		return
	}

	localVersion, err := u.bootUpdater.CheckLocalAssets()
	if err != nil {
		u.logger.Printf("[boot-local] failed to check local boot assets: %v", err)
		return
	}
	if localVersion == "" {
		u.logger.Printf("[boot-local] no local boot assets found")
		return
	}

	installedVersion, err := u.bootUpdater.GetInstalledVersion()
	if err != nil {
		u.logger.Printf("[boot-local] failed to read installed version: %v", err)
		return
	}
	if installedVersion == localVersion {
		u.logger.Printf("[boot-local] boot already at %s", localVersion)
		return
	}

	u.logger.Printf("[boot-local] local boot assets differ (installed=%s, local=%s), applying", installedVersion, localVersion)

	bootComp := bootComponent(u.config.Component)

	// For DBC: tell vehicle-service to keep dashboard power on during boot write.
	// This is critical — there is no A/B redundancy for the boot partition,
	// so a power cut mid-write could brick the device.
	if u.config.Component == "dbc" {
		if err := u.redis.PushUpdateCommand("start-dbc"); err != nil {
			u.logger.Printf("[boot-local] ABORTING: failed to send start-dbc — cannot guarantee power safety: %v", err)
			return
		}
	}

	if err := u.bootStatus.SetInstalling(u.ctx); err != nil {
		u.logger.Printf("[boot-local] failed to set installing status: %v", err)
	}

	if err := u.inhibitor.AddInstallInhibit(bootComp); err != nil {
		u.logger.Printf("[boot-local] failed to add install inhibit: %v", err)
	}

	if err := u.bootUpdater.Apply(u.ctx, boot.LocalAssetsPath); err != nil {
		u.logger.Printf("[boot-local] apply failed: %v", err)
		if err := u.inhibitor.RemoveInstallInhibit(bootComp); err != nil {
			u.logger.Printf("[boot-local] failed to remove install inhibit: %v", err)
		}
		if err := u.bootStatus.SetError(u.ctx, "install-failed", err.Error()); err != nil {
			u.logger.Printf("[boot-local] failed to set error status: %v", err)
		}
		if u.config.Component == "dbc" {
			if err := u.redis.PushUpdateCommand("complete-dbc"); err != nil {
				u.logger.Printf("[boot-local] failed to send complete-dbc after error: %v", err)
			}
		}
		return
	}

	if err := u.bootUpdater.WriteVersionFile(localVersion); err != nil {
		u.logger.Printf("[boot-local] failed to write version file: %v", err)
	}

	if err := u.inhibitor.RemoveInstallInhibit(bootComp); err != nil {
		u.logger.Printf("[boot-local] failed to remove install inhibit: %v", err)
	}

	// Boot write complete — release vehicle-service power protection
	if u.config.Component == "dbc" {
		if err := u.redis.PushUpdateCommand("complete-dbc"); err != nil {
			u.logger.Printf("[boot-local] failed to send complete-dbc: %v", err)
		}
	}

	if err := u.bootStatus.SetPendingReboot(u.ctx); err != nil {
		u.logger.Printf("[boot-local] failed to set pending-reboot status: %v", err)
	}

	u.logger.Printf("[boot-local] boot update applied, triggering reboot")
	if err := u.TriggerReboot(u.config.Component); err != nil {
		if !strings.Contains(err.Error(), "DRY-RUN") {
			u.logger.Printf("[boot-local] reboot trigger failed: %v", err)
			if err := u.bootStatus.SetError(u.ctx, "reboot-failed", err.Error()); err != nil {
				u.logger.Printf("[boot-local] failed to set error status: %v", err)
			}
		} else {
			u.logger.Printf("[boot-local] dry-run: simulating post-reboot state")
			if err := u.bootStatus.SetIdle(u.ctx); err != nil {
				u.logger.Printf("[boot-local] failed to set idle status in dry run: %v", err)
			}
		}
	}
}

// buildDeltaChain builds a complete chain of deltas from currentVersion to the latest release
// Returns nil if already at latest version, or error if chain cannot be built
func (u *Updater) buildDeltaChain(releases []Release, currentVersion, channel, variantID string) ([]Release, error) {
	// Filter and sort releases for our channel and variant
	var candidateReleases []Release
	for _, release := range releases {
		// Check if the release is for the specified channel.
		// Mirrors findLatestRelease: stable uses non-prerelease "v*" tags,
		// nightly/testing use prerelease tags with the channel- prefix.
		match := false
		switch channel {
		case "nightly":
			match = release.Prerelease && strings.HasPrefix(release.TagName, "nightly-")
		case "testing":
			match = release.Prerelease && strings.HasPrefix(release.TagName, "testing-")
		case "stable":
			match = !release.Prerelease && strings.HasPrefix(release.TagName, "v")
		default:
			match = strings.HasPrefix(release.TagName, channel+"-")
		}
		if !match {
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

	// Sort releases ascending. Stable tags (vX.Y.Z) need numeric comparison
	// — lexicographic puts v0.10.0 before v0.2.0. Nightly/testing tags embed
	// a sortable timestamp (channel-YYYYMMDDThhmmss), so lex is correct.
	sort.Slice(candidateReleases, func(i, j int) bool {
		if channel == "stable" {
			return compareVersions(candidateReleases[i].TagName, candidateReleases[j].TagName) < 0
		}
		return strings.ToLower(candidateReleases[i].TagName) < strings.ToLower(candidateReleases[j].TagName)
	})

	// Find where we are in the chain
	currentTag := strings.ToLower(currentVersion)
	if channel == "stable" {
		if !strings.HasPrefix(currentTag, "v") {
			currentTag = "v" + currentTag
		}
	} else if !strings.HasPrefix(currentTag, channel+"-") {
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
		newer := false
		if channel == "stable" {
			newer = compareVersions(release.TagName, currentTag) > 0
		} else {
			newer = strings.ToLower(release.TagName) > currentTag
		}
		if foundCurrent && newer {
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

func (u *Updater) getOrchestrateDBC() bool {
	u.orchestrateDBCMu.RLock()
	defer u.orchestrateDBCMu.RUnlock()
	return u.orchestrateDBCEnabled
}

func (u *Updater) setOrchestrateDBC(enabled bool) {
	u.orchestrateDBCMu.Lock()
	defer u.orchestrateDBCMu.Unlock()
	if u.orchestrateDBCEnabled != enabled {
		u.logger.Printf("DBC orchestration changed: %v -> %v", u.orchestrateDBCEnabled, enabled)
		u.orchestrateDBCEnabled = enabled
	}
}

// resolveOrchestrateDBC determines the effective value for orchestrate-dbc.
// Priority: explicit Redis setting > channel-dependent default.
func (u *Updater) resolveOrchestrateDBC() bool {
	val, isSet := u.redis.GetOrchestrateDBC()
	if isSet {
		return val == "true"
	}
	// Default: true for nightly, false otherwise
	return u.config.Channel == "nightly"
}
