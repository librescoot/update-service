// Package power requests CPU governor changes from pm-service via the
// scooter:governor list. Power inhibits live in internal/inhibitor.
package power

import (
	"context"
	"fmt"
	"log"

	"github.com/redis/go-redis/v9"
)

// PowerGovernorListKey is the pm-service command list for governor changes.
const PowerGovernorListKey = "scooter:governor"

// Client requests CPU governor changes from pm-service.
type Client struct {
	client *redis.Client
	ctx    context.Context
	logger *log.Logger
}

// New creates a new power manager client
func New(redisAddr string, logger *log.Logger) (*Client, error) {
	client := redis.NewClient(&redis.Options{
		Addr: redisAddr,
		DB:   0,
	})

	ctx := context.Background()
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

// RequestOndemandGovernor asks pm-service to switch to the ondemand governor
// so downloads and delta application don't run at powersave clocks.
func (c *Client) RequestOndemandGovernor() error {
	c.logger.Printf("Requesting CPU governor change to: ondemand")

	if _, err := c.client.LPush(c.ctx, PowerGovernorListKey, "ondemand").Result(); err != nil {
		return fmt.Errorf("failed to request governor change: %w", err)
	}

	return nil
}
