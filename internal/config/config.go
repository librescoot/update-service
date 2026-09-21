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

	Component       string // CLI-only target: mdb or dbc.
	Channel         string // stable, testing, or nightly; CLI overrides Redis.
	ChannelFromCLI  bool   // Immutable after startup; prevents Redis overriding --channel.
	FallbackChannel string // Startup default/inferred channel, never replaced by Redis.
	channelMu       sync.RWMutex

	DownloadDir string // CLI-only OTA staging directory.

	MdbRebootCheckInterval time.Duration
	UpdateRetryInterval    time.Duration

	// Download budget. Each bounds a single attempt; 0 disables that bound.
	// A budget abort keeps the partial file, so the next attempt resumes.
	//
	// These three are read once per download attempt through DownloadBudget()
	// by the download goroutine, while ApplyRedisUpdate can rewrite them at
	// any time from the settings-watcher goroutine. budgetMu guards those
	// accesses independently of channelMu.
	budgetMu              sync.RWMutex
	DownloadMaxDuration   time.Duration
	DownloadStallWindow   time.Duration
	DownloadStallMinBytes int64

	DryRun bool // Do not reboot; notify only.

	// Commit gate. When enabled, a pending Mender update is committed only
	// after the platform has proven itself on the new image, so a commit means
	// "booted and healthy" rather than only "the running version matches the
	// pending artifact". See internal/commitgate.
	//
	// Written by ApplyRedisUpdate and read once per probe tick by the gate
	// goroutine, so gateMu guards it independently of channelMu and budgetMu.
	gateMu                  sync.RWMutex
	CommitGateEnabled       bool
	CommitGateFloor         time.Duration
	CommitGateDeadline      time.Duration
	CommitGateRequiredUnits []string

	BootEnabled    bool
	BootMountPoint string
	BootDevice     string
	BootUBootSeek  int64 // 512-byte blocks before the U-Boot image.
}

const (
	// DefaultCommitGateFloor is how long the image must have been up before the
	// probes are evaluated. The floor is not a probe of its own: it keeps a
	// boot that is still assembling itself from being judged.
	DefaultCommitGateFloor = 3 * time.Minute
	// DefaultCommitGateDeadline is how long the gate may wait for every probe
	// before it fails closed and rolls the update back.
	DefaultCommitGateDeadline = 20 * time.Minute
)

// defaultCommitGateUnits lists the services a healthy boot must have brought
// up. Deliberately excluded: modem, uplink, battery, ecu and keycard. Those
// legitimately fail or are absent depending on SIM, card and fitted hardware,
// and a required unit that is wrongly listed turns a good update into a
// fail-closed rollback.
func defaultCommitGateUnits(component string) []string {
	// The MDB list is the one this service must have up to do its own work, and
	// every unit in it has been observed active on a healthy MDB. Modem, uplink,
	// battery, ecu and keycard are deliberately absent: those legitimately fail
	// or are absent depending on SIM, card and fitted hardware, and a required
	// unit that is wrongly listed turns a good update into a rollback.
	if component == "dbc" {
		// The DBC's own services, from the DBC image's package list: local valkey
		// (dbc-dispatcher and the dashboard talk to it), version-service, and the
		// dispatcher every DBC service is reached through. vehicle, settings and
		// pm-service live on the MDB, so they cannot be required here; what the DBC
		// needs from the MDB is checked separately by the vehicle probe.
		return []string{
			"valkey.service",
			"librescoot-version.service",
			"dbc-dispatcher.service",
		}
	}
	return []string{
		"valkey.service",
		"librescoot-vehicle.service",
		"librescoot-settings.service",
		"librescoot-version.service",
		"librescoot-pm.service",
	}
}

// CommitGateSettings is one coherent snapshot of the gate configuration. The
// gate reads it once per tick so a settings change applies to the next
// evaluation rather than partway through one.
type CommitGateSettings struct {
	Enabled       bool
	Floor         time.Duration
	Deadline      time.Duration
	RequiredUnits []string
}

// CommitGateSettings returns the current gate configuration.
func (c *Config) CommitGateSettings() CommitGateSettings {
	c.gateMu.RLock()
	defer c.gateMu.RUnlock()
	return CommitGateSettings{
		Enabled:       c.CommitGateEnabled,
		Floor:         c.CommitGateFloor,
		Deadline:      c.CommitGateDeadline,
		RequiredUnits: slices.Clone(c.CommitGateRequiredUnits),
	}
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
		FallbackChannel:        channel,
		DownloadDir:            downloadDir,
		MdbRebootCheckInterval: 5 * time.Minute,
		UpdateRetryInterval:    15 * time.Minute,
		DownloadMaxDuration:    60 * time.Minute,
		DownloadStallWindow:    2 * time.Minute,
		DownloadStallMinBytes:  64 * 1024,
		DryRun:                 dryRun,
		// Off unless a device opts in: enabling it changes the commit failure
		// mode to fail-closed, which rolls an image back.
		CommitGateEnabled:       false,
		CommitGateFloor:         DefaultCommitGateFloor,
		CommitGateDeadline:      DefaultCommitGateDeadline,
		CommitGateRequiredUnits: defaultCommitGateUnits(component),
		BootEnabled:             bootEnabled,
		BootMountPoint:          bootMountPoint,
		BootDevice:              bootDevice,
		BootUBootSeek:           bootUBootSeek,
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

	if v, err := redis.HGet(SettingsHashKey, prefix+"commit-gate"); err == nil && v != "" {
		if enabled, err := strconv.ParseBool(v); err == nil {
			c.CommitGateEnabled = enabled
		}
	}
	if v, err := redis.HGet(SettingsHashKey, prefix+"commit-gate-floor"); err == nil && v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			c.CommitGateFloor = d
		}
	}
	if v, err := redis.HGet(SettingsHashKey, prefix+"commit-gate-deadline"); err == nil && v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			c.CommitGateDeadline = d
		}
	}
	if v, err := redis.HGet(SettingsHashKey, prefix+"commit-gate-required-units"); err == nil && v != "" {
		if units := ParseUnitList(v); len(units) > 0 {
			c.CommitGateRequiredUnits = units
		}
	}

	return nil
}

// ParseUnitList splits a systemd unit list on commas and whitespace, so both
// "a.service,b.service" and "a.service b.service" are accepted. Entries
// without a recognized unit suffix are kept verbatim and rejected by systemd
// at probe time rather than silently dropped.
func ParseUnitList(value string) []string {
	fields := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	units := make([]string, 0, len(fields))
	for _, field := range fields {
		if field != "" {
			units = append(units, field)
		}
	}
	return units
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

// GetChannel returns the synchronized cached channel. Checks read Redis directly
// unless pinned by CLI; FallbackChannel is the separate startup default used
// when a Redis override is absent.
func (c *Config) GetChannel() string {
	c.channelMu.RLock()
	defer c.channelMu.RUnlock()
	return c.Channel
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
		if value == "" {
			value = c.FallbackChannel
		}
		if IsValidChannel(value) || value == "" {
			c.channelMu.Lock()
			c.Channel = value
			c.channelMu.Unlock()
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
	case "commit-gate":
		if enabled, err := strconv.ParseBool(value); err == nil {
			c.gateMu.Lock()
			c.CommitGateEnabled = enabled
			c.gateMu.Unlock()
			return true
		}
	case "commit-gate-floor":
		if d, err := time.ParseDuration(value); err == nil && d >= 0 {
			c.gateMu.Lock()
			c.CommitGateFloor = d
			c.gateMu.Unlock()
			return true
		}
	case "commit-gate-deadline":
		if d, err := time.ParseDuration(value); err == nil && d > 0 {
			c.gateMu.Lock()
			c.CommitGateDeadline = d
			c.gateMu.Unlock()
			return true
		}
	case "commit-gate-required-units":
		units := ParseUnitList(value)
		if len(units) == 0 {
			units = defaultCommitGateUnits(c.Component)
		}
		c.gateMu.Lock()
		c.CommitGateRequiredUnits = units
		c.gateMu.Unlock()
		return true
	}

	return false
}
