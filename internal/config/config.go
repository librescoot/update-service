package config

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	OtaStatusHashKey = "ota"
	OtaChannel       = "ota"
	VehicleHashKey   = "vehicle"
	OsReleaseHashKey = "os-release"
	SettingsHashKey  = "settings"
	SettingsChannel  = "settings"
)

type Config struct {
	RedisAddr string // CLI-only; never accepted from Redis.

	ReleasesURL   string
	CheckInterval time.Duration

	Component string // CLI-only target: mdb or dbc.
	Channel   string // stable, testing, or nightly; CLI overrides Redis.

	DownloadDir string // CLI-only OTA staging directory.

	MdbRebootCheckInterval time.Duration
	UpdateRetryInterval    time.Duration

	// Download budget. Each bounds a single attempt; 0 disables that bound.
	// A budget abort keeps the partial file, so the next attempt resumes.
	//
	// These three are read once per download attempt through DownloadBudget()
	// by the download goroutine, while ApplyRedisUpdate can rewrite them at
	// any time from the settings-watcher goroutine. budgetMu guards exactly
	// that pair of accesses. Every other Config field is read and written
	// directly with no synchronization, a convention of this struct that this
	// lock does not attempt to fix; these three differ in having a genuine
	// concurrent reader, so their guard has to be correct.
	budgetMu              sync.RWMutex
	DownloadMaxDuration   time.Duration
	DownloadStallWindow   time.Duration
	DownloadStallMinBytes int64

	DryRun bool // Do not reboot; notify only.

	BootEnabled    bool
	BootMountPoint string
	BootDevice     string
	BootUBootSeek  int64 // 512-byte blocks before the U-Boot image.
}

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
	bootUBootSeek int64,
) *Config {
	return &Config{
		RedisAddr:              redisAddr,
		ReleasesURL:            releasesURL,
		CheckInterval:          checkInterval,
		Component:              component,
		Channel:                channel,
		DownloadDir:            downloadDir,
		MdbRebootCheckInterval: 5 * time.Minute,
		UpdateRetryInterval:    15 * time.Minute,
		DownloadMaxDuration:    60 * time.Minute,
		DownloadStallWindow:    2 * time.Minute,
		DownloadStallMinBytes:  64 * 1024,
		DryRun:                 dryRun,
		BootEnabled:            bootEnabled,
		BootMountPoint:         bootMountPoint,
		BootDevice:             bootDevice,
		BootUBootSeek:          bootUBootSeek,
	}
}

func IsValidComponent(component string) bool {
	return component == "mdb" || component == "dbc"
}

func IsValidChannel(channel string) bool {
	validChannels := []string{"stable", "testing", "nightly"}
	return slices.Contains(validChannels, channel)
}

// InferChannelFromVersion maps installed artifact naming to a release channel.
func InferChannelFromVersion(version string) string {
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

type RedisSettings interface {
	HGet(key, field string) (string, error)
}

// LoadFromRedis loads configuration from Redis settings hash with component-specific prefix.
// Priority: CLI flags (if non-default) > Redis > hardcoded defaults.
// component and redisAddr are never loaded from Redis (CLI-only).
func (c *Config) LoadFromRedis(redis RedisSettings) error {
	prefix := fmt.Sprintf("updates.%s.", c.Component)

	if channel, err := redis.HGet(SettingsHashKey, prefix+"channel"); err == nil && channel != "" {
		if IsValidChannel(channel) {
			c.Channel = channel
		}
	}

	if intervalStr, err := redis.HGet(SettingsHashKey, prefix+"check-interval"); err == nil && intervalStr != "" {
		if intervalStr == "never" {
			c.CheckInterval = 0 // Zero disables automatic update checks.
		} else if duration, err := time.ParseDuration(intervalStr); err == nil {
			c.CheckInterval = duration
		}
	}

	if url, err := redis.HGet(SettingsHashKey, prefix+"releases-url"); err == nil && url != "" {
		c.ReleasesURL = url
	}

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
