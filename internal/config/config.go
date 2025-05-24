package config

import (
	"strings"
	"time"
)

// Fixed keys for Redis
const (
	OtaStatusHashKey = "ota"
	OtaChannel       = "ota"
	VehicleHashKey   = "vehicle"
)

// Config holds the configuration for the update service
type Config struct {
	// Redis configuration
	RedisAddr string

	// GitHub Releases API configuration
	GitHubReleasesURL string
	CheckInterval     time.Duration

	// Channel configuration
	DefaultChannel string // "stable", "testing", "nightly"

	// Component configuration
	Components []string // "dbc", "mdb"

	// Update configuration
	DbcUpdateKey string // "update:install:dbc" (contains URL#checksum)
	MdbUpdateKey string // "update:install:mdb" (contains URL#checksum)

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
	defaultChannel string,
	componentsStr string,
	dbcUpdateKey string,
	mdbUpdateKey string,
	dryRun bool,
) *Config {
	// Parse components string into slice
	components := strings.Split(componentsStr, ",")
	for i, c := range components {
		components[i] = strings.TrimSpace(c)
	}

	return &Config{
		RedisAddr:         redisAddr,
		GitHubReleasesURL: githubReleasesURL,
		CheckInterval:     checkInterval,
		DefaultChannel:    defaultChannel,
		Components:        components,
		DbcUpdateKey:      dbcUpdateKey,
		MdbUpdateKey:      mdbUpdateKey,
		// Default values for update constraints
		MdbRebootCheckInterval: 5 * time.Minute,
		UpdateRetryInterval:    15 * time.Minute,
		// Operational modes
		DryRun: dryRun,
	}
}

// IsValidComponent checks if the given component is valid
func (c *Config) IsValidComponent(component string) bool {
	for _, comp := range c.Components {
		if comp == component {
			return true
		}
	}
	return false
}

// IsValidChannel checks if the given channel is valid
func (c *Config) IsValidChannel(channel string) bool {
	// Currently supported channels
	validChannels := []string{"stable", "testing", "nightly"}
	for _, ch := range validChannels {
		if ch == channel {
			return true
		}
	}
	return false
}
