package config

import (
	"time"
)

// Fixed keys for Redis
const (
	OtaStatusHashKey   = "ota"
	OtaChannel         = "ota"
	VehicleHashKey     = "vehicle"
	OsReleaseHashKey   = "os-release"
)

// Config holds the configuration for the update service
type Config struct {
	// Redis configuration
	RedisAddr string

	// GitHub Releases API configuration
	GitHubReleasesURL string
	CheckInterval     time.Duration

	// Component and channel configuration
	Component string // "mdb" or "dbc" - which component this instance manages
	Channel   string // "stable", "testing", "nightly"

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
	dryRun bool,
) *Config {
	return &Config{
		RedisAddr:         redisAddr,
		GitHubReleasesURL: githubReleasesURL,
		CheckInterval:     checkInterval,
		Component:         component,
		Channel:           channel,
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
