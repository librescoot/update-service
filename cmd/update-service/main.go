package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/librescoot/update-service/internal/config"
	"github.com/librescoot/update-service/internal/inhibitor"
	"github.com/librescoot/update-service/internal/power"
	"github.com/librescoot/update-service/internal/redis"
	"github.com/librescoot/update-service/internal/updater"
)

var (
	redisAddr         = flag.String("redis-addr", "localhost:6379", "Redis server address")
	githubReleasesURL = flag.String("github-releases-url", "https://api.github.com/repos/librescoot/librescoot/releases", "GitHub Releases API URL")
	checkInterval     = flag.Duration("check-interval", 6*time.Hour, "Interval between update checks (use 0 or 'never' to disable)")
	component         = flag.String("component", "", "Component to manage updates for (mdb or dbc)")
	channel           = flag.String("channel", "nightly", "Update channel (stable, testing, nightly)")
	dryRun            = flag.Bool("dry-run", false, "If true, don't actually reboot, just notify")
)

func main() {
	flag.Parse()

	// Validate required component flag
	if *component == "" {
		log.Fatal("--component flag is required (mdb or dbc)")
	}
	if *component != "mdb" && *component != "dbc" {
		log.Fatalf("Invalid component '%s'. Must be 'mdb' or 'dbc'", *component)
	}

	// Set up logger
	var logger *log.Logger
	if os.Getenv("INVOCATION_ID") != "" {
		logger = log.New(os.Stdout, "", 0)
	} else {
		logger = log.New(os.Stdout, "librescoot-update: ", log.LstdFlags|log.Lmsgprefix)
	}
	logger.Printf("Starting update service for component: %s", *component)

	// Create context that can be cancelled on SIGINT or SIGTERM
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Set up signal handling
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		logger.Printf("Received signal: %v", sig)
		cancel()
	}()

	// Initialize Redis client early so we can load settings from it
	redisClient, err := redis.New(ctx, *redisAddr)
	if err != nil {
		logger.Fatalf("Failed to initialize Redis client: %v", err)
	}
	defer redisClient.Close()

	// Track which CLI flags were explicitly set (non-default)
	cliChannelSet := flag.Lookup("channel").Value.String() != flag.Lookup("channel").DefValue
	cliCheckIntervalSet := flag.Lookup("check-interval").Value.String() != flag.Lookup("check-interval").DefValue
	cliGithubURLSet := flag.Lookup("github-releases-url").Value.String() != flag.Lookup("github-releases-url").DefValue
	cliDryRunSet := flag.Lookup("dry-run").Value.String() != flag.Lookup("dry-run").DefValue

	// Always detect channel from installed version for logging/debugging purposes
	detectedChannel := ""
	installedVersion, err := redisClient.GetComponentVersion(*component)
	if err != nil {
		logger.Printf("Warning: Failed to get installed version for channel detection: %v", err)
	} else if installedVersion != "" {
		detectedChannel = config.InferChannelFromVersion(installedVersion)
		if detectedChannel != "" {
			logger.Printf("Detected channel '%s' from installed version: %s", detectedChannel, installedVersion)
		} else {
			logger.Printf("Could not infer channel from installed version: %s", installedVersion)
		}
	} else {
		logger.Printf("No installed version found")
	}

	// Determine the effective channel: CLI flag > Redis > detected > "nightly" default
	effectiveChannel := *channel
	if !cliChannelSet {
		if detectedChannel != "" {
			effectiveChannel = detectedChannel
		} else {
			effectiveChannel = "nightly"
		}
	}

	// Initialize config with CLI flags and detected/default values
	cfg := config.New(
		*redisAddr,
		*githubReleasesURL,
		*checkInterval,
		*component,
		effectiveChannel,
		*dryRun,
	)

	// Save CLI values if they were explicitly set
	cliChannel := cfg.Channel
	cliCheckInterval := cfg.CheckInterval
	cliGithubURL := cfg.GitHubReleasesURL
	cliDryRun := cfg.DryRun

	// Check if channel will come from Redis settings
	redisChannelSet := false
	if !cliChannelSet {
		if redisChannel, err := redisClient.HGet(config.SettingsHashKey, fmt.Sprintf("updates.%s.channel", *component)); err == nil && redisChannel != "" && config.IsValidChannel(redisChannel) {
			redisChannelSet = true
		}
	}

	// Load settings from Redis (will be overridden by CLI flags if they were set)
	if err := cfg.LoadFromRedis(redisClient); err != nil {
		logger.Printf("Warning: Failed to load settings from Redis: %v", err)
	}

	// Override with CLI flags if they were explicitly set (CLI takes precedence)
	if cliChannelSet {
		cfg.Channel = cliChannel
	}
	if cliCheckIntervalSet {
		cfg.CheckInterval = cliCheckInterval
	}
	if cliGithubURLSet {
		cfg.GitHubReleasesURL = cliGithubURL
	}
	if cliDryRunSet {
		cfg.DryRun = cliDryRun
	}

	// Start watching for settings changes in the background
	go watchSettingsChanges(ctx, redisClient, cfg, logger, cliChannelSet, cliCheckIntervalSet, cliGithubURLSet, cliDryRunSet)

	// Initialize power inhibitor client
	inhibitorClient, err := inhibitor.New(ctx, *redisAddr, logger)
	if err != nil {
		logger.Fatalf("Failed to initialize inhibitor client: %v", err)
	}
	defer inhibitorClient.Close()

	// Initialize power client
	powerClient, err := power.New(ctx, *redisAddr, logger)
	if err != nil {
		logger.Fatalf("Failed to initialize power client: %v", err)
	}
	defer powerClient.Close()

	// Initialize updater
	updater := updater.New(ctx, cfg, redisClient, inhibitorClient, powerClient, logger)
	defer updater.Close()

	// Check if there's a pending update that needs to be committed on startup
	if err := updater.CheckAndCommitPendingUpdate(); err != nil {
		logger.Printf("Warning: Failed to check/commit pending update: %v", err)
	}

	if err := updater.Start(); err != nil {
		logger.Fatalf("Failed to start updater: %v", err)
	}

	logger.Printf("Update service initialized with:")
	logger.Printf("  Redis address: %s", cfg.RedisAddr)
	logger.Printf("  GitHub Releases URL: %s", cfg.GitHubReleasesURL)
	logger.Printf("  Check interval: %v", cfg.CheckInterval)
	logger.Printf("  Component: %s", cfg.Component)

	// Log channel with source information
	if cliChannelSet {
		logger.Printf("  Channel: %s (explicitly set via CLI flag)", cfg.Channel)
	} else if redisChannelSet {
		logger.Printf("  Channel: %s (from Redis settings)", cfg.Channel)
	} else if detectedChannel != "" {
		logger.Printf("  Channel: %s (detected from installed version)", cfg.Channel)
	} else {
		logger.Printf("  Channel: %s (default)", cfg.Channel)
	}

	logger.Printf("  Dry-run mode: %v", cfg.DryRun)

	// Wait for context cancellation
	<-ctx.Done()
	logger.Printf("Shutting down update service")
}

// watchSettingsChanges monitors Redis for settings changes and applies them to the config
func watchSettingsChanges(ctx context.Context, redisClient *redis.Client, cfg *config.Config, logger *log.Logger, cliChannelSet, cliCheckIntervalSet, cliGithubURLSet, cliDryRunSet bool) {
	msgChan, cleanup, err := redisClient.SubscribeToSettingsChanges(config.SettingsChannel)
	if err != nil {
		logger.Printf("Warning: Failed to subscribe to settings changes: %v", err)
		return
	}
	defer cleanup()

	logger.Printf("Watching for settings changes on channel '%s'", config.SettingsChannel)

	for {
		select {
		case <-ctx.Done():
			return
		case settingKey, ok := <-msgChan:
			if !ok {
				logger.Printf("Settings channel closed")
				return
			}

			logger.Printf("Settings change notification received for key: %s", settingKey)

			// Get the new value from Redis
			value, err := redisClient.HGet(config.SettingsHashKey, settingKey)
			if err != nil {
				logger.Printf("Warning: Failed to read setting %s from Redis: %v", settingKey, err)
				continue
			}

			// Skip applying settings that were overridden by CLI flags
			prefix := "updates." + cfg.Component + "."
			if len(settingKey) > len(prefix) && settingKey[:len(prefix)] == prefix {
				settingName := settingKey[len(prefix):]
				switch settingName {
				case "channel":
					if cliChannelSet {
						logger.Printf("Ignoring Redis update for channel (overridden by CLI flag)")
						continue
					}
				case "check-interval":
					if cliCheckIntervalSet {
						logger.Printf("Ignoring Redis update for check-interval (overridden by CLI flag)")
						continue
					}
				case "github-releases-url":
					if cliGithubURLSet {
						logger.Printf("Ignoring Redis update for github-releases-url (overridden by CLI flag)")
						continue
					}
				case "dry-run":
					if cliDryRunSet {
						logger.Printf("Ignoring Redis update for dry-run (overridden by CLI flag)")
						continue
					}
				}
			}

			// Apply the setting update
			if cfg.ApplyRedisUpdate(settingKey, value) {
				logger.Printf("Applied setting update: %s = %s", settingKey, value)

				// If check-interval was updated, evaluate if we should check now
				if len(settingKey) > len(prefix) && settingKey[:len(prefix)] == prefix {
					settingName := settingKey[len(prefix):]
					if settingName == "check-interval" {
						evaluateCheckIntervalChange(ctx, redisClient, cfg, logger)
					}
				}
			}
		}
	}
}

// evaluateCheckIntervalChange evaluates if an update check should be triggered based on the new check interval
func evaluateCheckIntervalChange(ctx context.Context, redisClient *redis.Client, cfg *config.Config, logger *log.Logger) {
	// If automated checks are disabled (interval is 0), don't trigger a check
	if cfg.CheckInterval == 0 {
		logger.Printf("Check interval set to 0 (disabled), not triggering check")
		return
	}

	// Get the last check time from Redis
	lastCheckTime, err := redisClient.GetLastUpdateCheckTime(cfg.Component)
	if err != nil {
		logger.Printf("Warning: Failed to get last check time: %v. Will not trigger immediate check.", err)
		return
	}

	// If there's no recorded last check time, don't trigger a check
	// The normal update loop will handle it
	if lastCheckTime.IsZero() {
		logger.Printf("No previous check time recorded, will wait for normal check interval")
		return
	}

	// Calculate time since last check
	timeSinceLastCheck := time.Since(lastCheckTime)
	logger.Printf("Time since last check: %v, new check interval: %v", timeSinceLastCheck, cfg.CheckInterval)

	// If enough time has passed based on the new interval, trigger a check
	if timeSinceLastCheck >= cfg.CheckInterval {
		logger.Printf("Time since last check (%v) >= new interval (%v), triggering immediate check", timeSinceLastCheck, cfg.CheckInterval)
		if err := redisClient.PushUpdateCommandToComponent(cfg.Component, "check-now"); err != nil {
			logger.Printf("Warning: Failed to trigger update check: %v", err)
		}
	} else {
		remainingTime := cfg.CheckInterval - timeSinceLastCheck
		logger.Printf("Time since last check (%v) < new interval (%v), next check in %v", timeSinceLastCheck, cfg.CheckInterval, remainingTime)
	}
}
