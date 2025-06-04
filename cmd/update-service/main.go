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
	"github.com/librescoot/update-service/internal/redis"
	"github.com/librescoot/update-service/internal/updater"
)

var (
	redisAddr         = flag.String("redis-addr", "localhost:6379", "Redis server address")
	githubReleasesURL = flag.String("github-releases-url", "https://api.github.com/repos/librescoot/librescoot/releases", "GitHub Releases API URL")
	checkInterval     = flag.Duration("check-interval", 6*time.Hour, "Interval between update checks")
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
	logger := log.New(os.Stdout, "update-service: ", log.LstdFlags)
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

	// Initialize config
	cfg := config.New(
		*redisAddr,
		*githubReleasesURL,
		*checkInterval,
		*component,
		*channel,
		*dryRun,
	)

	// Initialize Redis client
	redisClient, err := redis.New(ctx, *redisAddr)
	if err != nil {
		logger.Fatalf("Failed to initialize Redis client: %v", err)
	}
	defer redisClient.Close()

	// Initialize power inhibitor client
	inhibitorClient, err := inhibitor.New(ctx, *redisAddr, logger)
	if err != nil {
		logger.Fatalf("Failed to initialize inhibitor client: %v", err)
	}
	defer inhibitorClient.Close()

	// Initialize updater
	updater := updater.New(ctx, cfg, redisClient, inhibitorClient, logger)
	defer updater.Close()

	// Check if there's a pending update that needs to be committed on startup
	if err := updater.CheckAndCommitPendingUpdate(); err != nil {
		logger.Printf("Warning: Failed to check/commit pending update: %v", err)
	}

	if err := updater.Start(); err != nil {
		logger.Fatalf("Failed to start updater: %v", err)
	}

	logger.Printf("Update service initialized with:")
	logger.Printf("  Redis address: %s", *redisAddr)
	logger.Printf("  GitHub Releases URL: %s", *githubReleasesURL)
	logger.Printf("  Check interval: %v", *checkInterval)
	logger.Printf("  Component: %s", *component)
	logger.Printf("  Channel: %s", *channel)
	logger.Printf("  Dry-run mode: %v", *dryRun)

	// Wait for context cancellation
	<-ctx.Done()
	logger.Printf("Shutting down update service")
}
