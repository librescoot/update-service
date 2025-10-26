package main

import (
	"context"
	"flag"
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

	// Initialize config with CLI flags and defaults
	cfg := config.New(
		*redisAddr,
		*githubReleasesURL,
		*checkInterval,
		*component,
		*channel,
		*dryRun,
	)

	// Track which CLI flags were explicitly set (non-default)
	cliChannelSet := flag.Lookup("channel").Value.String() != flag.Lookup("channel").DefValue
	cliCheckIntervalSet := flag.Lookup("check-interval").Value.String() != flag.Lookup("check-interval").DefValue
	cliGithubURLSet := flag.Lookup("github-releases-url").Value.String() != flag.Lookup("github-releases-url").DefValue
	cliDryRunSet := flag.Lookup("dry-run").Value.String() != flag.Lookup("dry-run").DefValue

	// Save CLI values if they were explicitly set
	cliChannel := cfg.Channel
	cliCheckInterval := cfg.CheckInterval
	cliGithubURL := cfg.GitHubReleasesURL
	cliDryRun := cfg.DryRun

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
	logger.Printf("  Channel: %s", cfg.Channel)
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
			}
		}
	}
}
