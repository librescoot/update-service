package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Fixed keys for Redis
const (
	OtaStatusHashKey   = "ota"
	OtaChannel         = "ota"
	VehicleHashKey     = "vehicle"
	OsReleaseHashKey   = "os-release"
	SettingsHashKey    = "settings"
	SettingsChannel    = "settings"
)

// Config holds the configuration for the update service
type Config struct {
	// Redis configuration (CLI-only, never from Redis)
	RedisAddr string

	// GitHub Releases API configuration
	GitHubReleasesURL string
	CheckInterval     time.Duration

	// Component and channel configuration
	Component string // "mdb" or "dbc" - which component this instance manages (CLI-only, never from Redis)
	Channel   string // "stable", "testing", "nightly"

	// Download directory (CLI-only, never from Redis)
	DownloadDir string // Directory where OTA files are downloaded (default: /data/ota/{component})

	// Update constraints
	MdbRebootCheckInterval time.Duration // How often to check if MDB can be rebooted
	UpdateRetryInterval    time.Duration // How often to retry updates if conditions aren't met

	// Operational modes
	DryRun bool // If true, don't actually reboot, just notify
}

// New creates a new Config with the given parameters
func New(
	redisAddr string,
	githubReleasesURL string,
	checkInterval time.Duration,
	component string,
	channel string,
	downloadDir string,
	dryRun bool,
) *Config {
	return &Config{
		RedisAddr:         redisAddr,
		GitHubReleasesURL: githubReleasesURL,
		CheckInterval:     checkInterval,
		Component:         component,
		Channel:           channel,
		DownloadDir:       downloadDir,
		// Default values for update constraints
		MdbRebootCheckInterval: 5 * time.Minute,
		UpdateRetryInterval:    15 * time.Minute,
		// Operational modes
		DryRun: dryRun,
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
	for _, ch := range validChannels {
		if ch == channel {
			return true
		}
	}
	return false
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

	// Load github-releases-url from Redis if available
	if url, err := redis.HGet(SettingsHashKey, prefix+"github-releases-url"); err == nil && url != "" {
		c.GitHubReleasesURL = url
	}

	// Load dry-run from Redis if available
	if dryRunStr, err := redis.HGet(SettingsHashKey, prefix+"dry-run"); err == nil && dryRunStr != "" {
		if dryRun, err := strconv.ParseBool(dryRunStr); err == nil {
			c.DryRun = dryRun
		}
	}

	return nil
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
	case "github-releases-url":
		c.GitHubReleasesURL = value
		return true
	case "dry-run":
		if dryRun, err := strconv.ParseBool(value); err == nil {
			c.DryRun = dryRun
			return true
		}
	}

	return false
}
