package power

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// Redis keys for power inhibits
	InhibitHashKey = "power:inhibits"
	InhibitChannel = "power:inhibits"
)

// InhibitType represents the type of power inhibit
type InhibitType string

const (
	InhibitTypeDownloading InhibitType = "downloading" // Delay power state changes for up to 5 minutes
	InhibitTypeInstalling  InhibitType = "installing"  // Defer power state changes completely
)

// Client represents a Redis client for interacting with the power manager
type Client struct {
	client *redis.Client
	ctx    context.Context
	logger *log.Logger
}

// New creates a new power manager client
func New(ctx context.Context, redisAddr string, logger *log.Logger) (*Client, error) {
	client := redis.NewClient(&redis.Options{
		Addr: redisAddr,
		DB:   0,
	})

	// Test connection
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	return &Client{
		client: client,
		ctx:    ctx,
		logger: logger,
	}, nil
}

// Close closes the Redis client
func (c *Client) Close() error {
	return c.client.Close()
}

// AddInhibit adds a power inhibit
func (c *Client) AddInhibit(id string, inhibitType InhibitType, duration time.Duration) error {
	c.logger.Printf("Adding power inhibit: id=%s, type=%s, duration=%v", id, inhibitType, duration)

	// Create inhibit data
	inhibitData := map[string]interface{}{
		"id":       id,
		"type":     string(inhibitType),
		"duration": int64(duration.Seconds()),
		"created":  time.Now().Unix(),
	}

	// Add inhibit to Redis
	pipe := c.client.Pipeline()
	pipe.HSet(c.ctx, InhibitHashKey, id, inhibitData)
	pipe.Publish(c.ctx, InhibitChannel, fmt.Sprintf("add:%s", id))

	_, err := pipe.Exec(c.ctx)
	if err != nil {
		return fmt.Errorf("failed to add power inhibit: %w", err)
	}

	return nil
}

// RemoveInhibit removes a power inhibit
func (c *Client) RemoveInhibit(id string) error {
	c.logger.Printf("Removing power inhibit: id=%s", id)

	// Remove inhibit from Redis
	pipe := c.client.Pipeline()
	pipe.HDel(c.ctx, InhibitHashKey, id)
	pipe.Publish(c.ctx, InhibitChannel, fmt.Sprintf("remove:%s", id))

	_, err := pipe.Exec(c.ctx)
	if err != nil {
		return fmt.Errorf("failed to remove power inhibit: %w", err)
	}

	return nil
}

// AddDownloadInhibit adds a download inhibit that delays power state changes
// for up to 5 minutes while an update is downloading
func (c *Client) AddDownloadInhibit(componentID string) error {
	id := fmt.Sprintf("download:%s", componentID)
	return c.AddInhibit(id, InhibitTypeDownloading, 5*time.Minute)
}

// RemoveDownloadInhibit removes a download inhibit
func (c *Client) RemoveDownloadInhibit(componentID string) error {
	id := fmt.Sprintf("download:%s", componentID)
	return c.RemoveInhibit(id)
}

// AddInstallInhibit adds an install inhibit that defers power state changes
// completely while an update is being installed
func (c *Client) AddInstallInhibit(componentID string) error {
	id := fmt.Sprintf("install:%s", componentID)
	return c.AddInhibit(id, InhibitTypeInstalling, 0) // 0 duration means indefinite
}

// RemoveInstallInhibit removes an install inhibit
func (c *Client) RemoveInstallInhibit(componentID string) error {
	id := fmt.Sprintf("install:%s", componentID)
	return c.RemoveInhibit(id)
}
