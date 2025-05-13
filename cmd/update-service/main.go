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
	"github.com/librescoot/update-service/internal/vehicle"
)

var (
	redisAddr         = flag.String("redis-addr", "localhost:6379", "Redis server address")
	githubReleasesURL = flag.String("github-releases-url", "https://api.github.com/repos/librescoot/librescoot/releases", "GitHub Releases API URL")
	checkInterval     = flag.Duration("check-interval", 1*time.Hour, "Interval between update checks")
	defaultChannel    = flag.String("default-channel", "stable", "Default update channel (stable, testing, nightly)")
	components        = flag.String("components", "dbc,mdb", "Comma-separated list of components to check for updates")
	dbcUpdateKey      = flag.String("dbc-update-key", "mender/update/dbc/url", "Redis key for DBC update URLs")
	mdbUpdateKey      = flag.String("mdb-update-key", "mender/update/mdb/url", "Redis key for MDB update URLs")
	dbcChecksumKey    = flag.String("dbc-checksum-key", "mender/update/dbc/checksum", "Redis key for DBC update checksums")
	mdbChecksumKey    = flag.String("mdb-checksum-key", "mender/update/mdb/checksum", "Redis key for MDB update checksums")
	dryRun            = flag.Bool("dry-run", false, "If true, don't actually reboot, just notify")
)

func main() {
	flag.Parse()

	// Set up logger
	logger := log.New(os.Stdout, "update-service: ", log.LstdFlags)
	logger.Printf("Starting update service")

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
		*defaultChannel,
		*components,
		*dbcUpdateKey,
		*mdbUpdateKey,
		*dbcChecksumKey,
		*mdbChecksumKey,
		*dryRun,
	)

	// Initialize Redis client
	redisClient, err := redis.New(ctx, *redisAddr)
	if err != nil {
		logger.Fatalf("Failed to initialize Redis client: %v", err)
	}
	defer redisClient.Close()

	// Initialize vehicle service client
	vehicleService := vehicle.New(redisClient, config.VehicleHashKey, cfg.DryRun)

	// Initialize power inhibitor client
	inhibitorClient, err := inhibitor.New(ctx, *redisAddr, logger)
	if err != nil {
		logger.Fatalf("Failed to initialize inhibitor client: %v", err)
	}
	defer inhibitorClient.Close()
	
	// Initialize power management client
	powerClient, err := power.New(ctx, *redisAddr, logger)
	if err != nil {
		logger.Fatalf("Failed to initialize power client: %v", err)
	}
	defer powerClient.Close()

	// Initialize updater
	updater := updater.New(ctx, cfg, redisClient, vehicleService, inhibitorClient, powerClient, logger)
	if err := updater.Start(); err != nil {
		logger.Fatalf("Failed to start updater: %v", err)
	}

	logger.Printf("Update service initialized with:")
	logger.Printf("  Redis address: %s", *redisAddr)
	logger.Printf("  GitHub Releases URL: %s", *githubReleasesURL)
	logger.Printf("  Check interval: %v", *checkInterval)
	logger.Printf("  Default channel: %s", *defaultChannel)
	logger.Printf("  Components: %s", *components)
	logger.Printf("  Dry-run mode: %v", *dryRun)

	// Wait for context cancellation
	<-ctx.Done()
	logger.Printf("Shutting down update service")
}
