package redis

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Client represents a Redis client for the update service
type Client struct {
	client *redis.Client
	ctx    context.Context
}

// New creates a new Redis client
func New(ctx context.Context, addr string) (*Client, error) {
	client := redis.NewClient(&redis.Options{
		Addr: addr,
		DB:   0,
	})

	// Test connection
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	return &Client{
		client: client,
		ctx:    ctx,
	}, nil
}

// Close closes the Redis client
func (c *Client) Close() error {
	return c.client.Close()
}

// SetVehicleState sets the vehicle state in Redis
func (c *Client) SetVehicleState(vehicleHashKey, state string) error {
	return c.client.HSet(c.ctx, vehicleHashKey, "state", state).Err()
}

// GetVehicleState gets the vehicle state from Redis
func (c *Client) GetVehicleState(vehicleHashKey string) (string, error) {
	return c.client.HGet(c.ctx, vehicleHashKey, "state").Result()
}

// PushUpdateURL pushes an update URL to the specified Redis key
func (c *Client) PushUpdateURL(updateKey, url string) error {
	return c.client.LPush(c.ctx, updateKey, url).Err()
}

// SetUpdateChecksum sets the update checksum in Redis
func (c *Client) SetUpdateChecksum(checksumKey, checksum string) error {
	return c.client.Set(c.ctx, checksumKey, checksum, 0).Err()
}

// GetOTAStatus gets the OTA status from Redis
func (c *Client) GetOTAStatus(otaHashKey string) (map[string]string, error) {
	return c.client.HGetAll(c.ctx, otaHashKey).Result()
}

// WaitForOTAStatus waits for the OTA status to match the expected status
// It polls the OTA status hash at the specified interval until the status matches
// or the context is cancelled
func (c *Client) WaitForOTAStatus(otaHashKey, statusField, expectedStatus string, pollInterval time.Duration) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return c.ctx.Err()
		case <-ticker.C:
			status, err := c.client.HGet(c.ctx, otaHashKey, statusField).Result()
			if err != nil {
				if err == redis.Nil {
					// Status field doesn't exist yet, continue polling
					continue
				}
				return err
			}

			if status == expectedStatus {
				return nil
			}
		}
	}
}
