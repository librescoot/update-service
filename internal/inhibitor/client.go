package inhibitor

import (
	"context"
	"encoding/json"
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
	TypeBlock InhibitType = "block" // Block power state changes completely
	TypeDelay InhibitType = "delay" // Delay power state changes for a specified duration
)

// InhibitData represents the data stored in Redis for an inhibit
type InhibitData struct {
	ID       string      `json:"id"`
	Who      string      `json:"who"`
	What     string      `json:"what"`
	Why      string      `json:"why"`
	Type     InhibitType `json:"type"`
	Duration int64       `json:"duration"`
	Created  int64       `json:"created"`
}

// Client represents a Redis client for interacting with the power inhibitor system
type Client struct {
	client *redis.Client
	ctx    context.Context
	logger *log.Logger
}

// New creates a new power inhibitor client
func New(redisAddr string, logger *log.Logger) (*Client, error) {
	client := redis.NewClient(&redis.Options{
		Addr: redisAddr,
		DB:   0,
	})

	ctx := context.Background()
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
func (c *Client) AddInhibit(id, who, what, why string, inhibitType InhibitType, duration time.Duration) error {
	c.logger.Printf("Adding power inhibit: id=%s, who=%s, what=%s, why=%s, type=%s, duration=%v",
		id, who, what, why, inhibitType, duration)

	// Create inhibit data
	inhibitData := &InhibitData{
		ID:       id,
		Who:      who,
		What:     what,
		Why:      why,
		Type:     inhibitType,
		Duration: int64(duration.Seconds()),
		Created:  time.Now().Unix(),
	}

	// Marshal to JSON
	data, err := json.Marshal(inhibitData)
	if err != nil {
		return fmt.Errorf("failed to marshal inhibit data: %w", err)
	}

	// Add inhibit to Redis
	pipe := c.client.Pipeline()
	pipe.HSet(c.ctx, InhibitHashKey, id, string(data))
	pipe.Publish(c.ctx, InhibitChannel, fmt.Sprintf("add:%s", id))

	_, err = pipe.Exec(c.ctx)
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
	who := "update-service"
	what := "power-state-change"
	why := fmt.Sprintf("downloading update for %s", componentID)
	return c.AddInhibit(id, who, what, why, TypeDelay, 5*time.Minute)
}

// RemoveDownloadInhibit removes a download inhibit
func (c *Client) RemoveDownloadInhibit(componentID string) error {
	id := fmt.Sprintf("download:%s", componentID)
	return c.RemoveInhibit(id)
}

// AddInstallInhibit adds an install inhibit that blocks power state changes
// completely while an update is being installed
func (c *Client) AddInstallInhibit(componentID string) error {
	id := fmt.Sprintf("install:%s", componentID)
	who := "update-service"
	what := "power-state-change"
	why := fmt.Sprintf("installing update for %s", componentID)
	return c.AddInhibit(id, who, what, why, TypeBlock, 0) // 0 duration means indefinite
}

// RemoveInstallInhibit removes an install inhibit
func (c *Client) RemoveInstallInhibit(componentID string) error {
	id := fmt.Sprintf("install:%s", componentID)
	return c.RemoveInhibit(id)
}
