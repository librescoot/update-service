package config

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Fixed keys for Redis
const (
	OtaStatusHashKey = "ota"
	OtaChannel       = "ota"
	VehicleHashKey   = "vehicle"
	OsReleaseHashKey = "os-release"
	SettingsHashKey  = "settings"
	SettingsChannel  = "settings"
)

// Config holds the configuration for the update service
type Config struct {
	// Redis configuration (CLI-only, never from Redis)
	RedisAddr string

	// GitHub Releases API configuration
	ReleasesURL   string
	CheckInterval time.Duration

	// Component and channel configuration
	Component string // "mdb" or "dbc" - which component this instance manages (CLI-only, never from Redis)
	Channel   string // "stable", "testing", "nightly"

	// Download directory (CLI-only, never from Redis)
	DownloadDir string // Directory where OTA files are downloaded (default: /data/ota/{component})

	// Update constraints
	MdbRebootCheckInterval time.Duration // How often to check if MDB can be rebooted
	UpdateRetryInterval    time.Duration // How often to retry updates if conditions aren't met

	// Download budget. Each bounds a single attempt; 0 disables that bound.
	// A budget abort keeps the partial file, so the next attempt resumes.
	//
	// These three are read concurrently, once per download attempt, through
	// DownloadBudget() by the download goroutine, while ApplyRedisUpdate can
	// rewrite them at any time from the settings-watcher goroutine. budgetMu
	// guards exactly that: every other Config field is read and written
	// directly with no synchronization at all, an existing convention of
	// this struct that this lock does not attempt to fix. These three fields
	// are different because, unlike Channel or CheckInterval, they never had
	// a concurrent reader before the budget became a live provider (the
	// value used to be copied once, at construction, into a Downloader field
	// that was then never re-read) — this lock is what gives them one, so it
	// needs to actually be correct.
	budgetMu              sync.RWMutex
	DownloadMaxDuration   time.Duration // Wall clock cap on one download attempt
	DownloadStallWindow   time.Duration // Rolling window the throughput floor is measured over
	DownloadStallMinBytes int64         // Bytes that must arrive within each window

	// Operational modes
	DryRun bool // If true, don't actually reboot, just notify

	// Boot partition update configuration (CLI-only)
	BootEnabled    bool   // Enable boot partition updates
	BootMountPoint string // Boot partition mount point (default: /uboot)
	BootDevice     string // U-Boot device path (auto-detected from mount if empty)
	BootDTBFile    string // DTB filename (default: librescoot-{component}.dtb)
	BootUBootSeek  int64  // 512-byte blocks to seek before writing U-Boot (default: 2)
}

// New creates a new Config with the given parameters
func New(
	redisAddr string,
	releasesURL string,
	checkInterval time.Duration,
	component string,
	channel string,
	downloadDir string,
	dryRun bool,
	bootEnabled bool,
	bootMountPoint string,
	bootDevice string,
	bootDTBFile string,
	bootUBootSeek int64,
) *Config {
	if bootDTBFile == "" {
		bootDTBFile = "librescoot-" + component + ".dtb"
	}
	return &Config{
		RedisAddr:     redisAddr,
		ReleasesURL:   releasesURL,
		CheckInterval: checkInterval,
		Component:     component,
		Channel:       channel,
		DownloadDir:   downloadDir,
		// Default values for update constraints
		MdbRebootCheckInterval: 5 * time.Minute,
		UpdateRetryInterval:    15 * time.Minute,
		// Default download budget
		DownloadMaxDuration:   60 * time.Minute,
		DownloadStallWindow:   2 * time.Minute,
		DownloadStallMinBytes: 64 * 1024,
		// Operational modes
		DryRun: dryRun,
		// Boot partition update
		BootEnabled:    bootEnabled,
		BootMountPoint: bootMountPoint,
		BootDevice:     bootDevice,
		BootDTBFile:    bootDTBFile,
		BootUBootSeek:  bootUBootSeek,
	}
}

// IsValidComponent checks if the given component is valid
func IsValidComponent(component string) bool {
	return component == "mdb" || component == "dbc"
}

// IsValidChannel checks if the given channel is valid
func IsValidChannel(channel string) bool {
	// Currently supported channels
	validChannels := []string{"stable", "testing", "nightly"}
	return slices.Contains(validChannels, channel)
}

// InferChannelFromVersion attempts to infer the channel from a version string.
// Returns empty string if channel cannot be determined.
func InferChannelFromVersion(version string) string {
	// Clean up version string (remove potential codename suffix like " (none)")
	version = strings.Split(version, " ")[0]

	if strings.HasPrefix(version, "nightly-") {
		return "nightly"
	}
	if strings.HasPrefix(version, "testing-") {
		return "testing"
	}
	if strings.HasPrefix(version, "v") || (len(version) > 0 && version[0] >= '0' && version[0] <= '9') {
		return "stable"
	}
	return ""
}

// RedisSettings defines the interface for reading settings from Redis
type RedisSettings interface {
	HGet(key, field string) (string, error)
}

// LoadFromRedis loads configuration from Redis settings hash with component-specific prefix.
// Priority: CLI flags (if non-default) > Redis > hardcoded defaults.
// component and redisAddr are never loaded from Redis (CLI-only).
func (c *Config) LoadFromRedis(redis RedisSettings) error {
	prefix := fmt.Sprintf("updates.%s.", c.Component)

	// Load channel from Redis if available
	if channel, err := redis.HGet(SettingsHashKey, prefix+"channel"); err == nil && channel != "" {
		if IsValidChannel(channel) {
			c.Channel = channel
		}
	}

	// Load check-interval from Redis if available
	if intervalStr, err := redis.HGet(SettingsHashKey, prefix+"check-interval"); err == nil && intervalStr != "" {
		if intervalStr == "never" {
			c.CheckInterval = 0 // 0 means disabled
		} else if duration, err := time.ParseDuration(intervalStr); err == nil {
			c.CheckInterval = duration
		}
	}

	// Load releases-url from Redis if available
	if url, err := redis.HGet(SettingsHashKey, prefix+"releases-url"); err == nil && url != "" {
		c.ReleasesURL = url
	}

	// Load dry-run from Redis if available
	if dryRunStr, err := redis.HGet(SettingsHashKey, prefix+"dry-run"); err == nil && dryRunStr != "" {
		if dryRun, err := strconv.ParseBool(dryRunStr); err == nil {
			c.DryRun = dryRun
		}
	}

	// Load download budget settings from Redis if available. 0 disables a
	// bound; time.ParseDuration("0") returns 0 with no error, so no special
	// case is needed for the disable value.
	if v, err := redis.HGet(SettingsHashKey, prefix+"download-max-duration"); err == nil && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.DownloadMaxDuration = d
		}
	}
	if v, err := redis.HGet(SettingsHashKey, prefix+"download-stall-window"); err == nil && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.DownloadStallWindow = d
		}
	}
	if v, err := redis.HGet(SettingsHashKey, prefix+"download-stall-min-bytes"); err == nil && v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			c.DownloadStallMinBytes = n
		}
	}

	return nil
}

// DownloadBudget returns a coherent snapshot of the three download-budget
// fields for a caller about to start a new download attempt. Called once per
// attempt so a setting change mid-transfer never disturbs a download already
// underway, only the next one.
func (c *Config) DownloadBudget() (maxDuration, stallWindow time.Duration, stallMinBytes int64) {
	c.budgetMu.RLock()
	defer c.budgetMu.RUnlock()
	return c.DownloadMaxDuration, c.DownloadStallWindow, c.DownloadStallMinBytes
}

// ApplyRedisUpdate applies a single setting update from Redis.
// Returns true if the setting was recognized and applied, false otherwise.
func (c *Config) ApplyRedisUpdate(key, value string) bool {
	prefix := fmt.Sprintf("updates.%s.", c.Component)

	// Only process settings for this component
	if len(key) <= len(prefix) || key[:len(prefix)] != prefix {
		return false
	}

	settingName := key[len(prefix):]

	switch settingName {
	case "channel":
		if IsValidChannel(value) {
			c.Channel = value
			return true
		}
	case "check-interval":
		if value == "never" {
			c.CheckInterval = 0 // 0 means disabled
			return true
		} else if duration, err := time.ParseDuration(value); err == nil {
			c.CheckInterval = duration
			return true
		}
	case "releases-url":
		c.ReleasesURL = value
		return true
	case "dry-run":
		if dryRun, err := strconv.ParseBool(value); err == nil {
			c.DryRun = dryRun
			return true
		}
	case "download-max-duration":
		if d, err := time.ParseDuration(value); err == nil {
			c.budgetMu.Lock()
			c.DownloadMaxDuration = d
			c.budgetMu.Unlock()
			return true
		}
	case "download-stall-window":
		if d, err := time.ParseDuration(value); err == nil {
			c.budgetMu.Lock()
			c.DownloadStallWindow = d
			c.budgetMu.Unlock()
			return true
		}
	case "download-stall-min-bytes":
		if n, err := strconv.ParseInt(value, 10, 64); err == nil && n >= 0 {
			c.budgetMu.Lock()
			c.DownloadStallMinBytes = n
			c.budgetMu.Unlock()
			return true
		}
	}

	return false
}
