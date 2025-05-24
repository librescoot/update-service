package redis

import (
	"context"
	"fmt"
	"time"

	"github.com/librescoot/update-service/pkg/config"
	"github.com/redis/go-redis/v9"
)

// Client wraps the Redis client with our specific methods
type Client struct {
	client *redis.Client
}

// NewClient creates a new Redis client
func NewClient(cfg *config.Config) *Client {
	client := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Addr,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	})

	return &Client{
		client: client,
	}
}

// Connect tests the connection to Redis
func (c *Client) Connect() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	
	_, err := c.client.Ping(ctx).Result()
	if err != nil {
		return fmt.Errorf("failed to connect to Redis: %w", err)
	}
	return nil
}

// Close closes the Redis connection
func (c *Client) Close() error {
	return c.client.Close()
}

// Get retrieves a value from Redis
func (c *Client) Get(key string) (string, error) {
	ctx := context.Background()
	val, err := c.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to get key %s: %w", key, err)
	}
	return val, nil
}

// Set stores a value in Redis
func (c *Client) Set(key, value string) error {
	ctx := context.Background()
	err := c.client.Set(ctx, key, value, 0).Err()
	if err != nil {
		return fmt.Errorf("failed to set key %s: %w", key, err)
	}
	return nil
}

// HSet stores a field-value pair in a Redis hash
func (c *Client) HSet(key, field, value string) error {
	ctx := context.Background()
	err := c.client.HSet(ctx, key, field, value).Err()
	if err != nil {
		return fmt.Errorf("failed to hset %s %s: %w", key, field, err)
	}
	return nil
}

// RPush pushes a value to the right end of a Redis list
func (c *Client) RPush(key, value string) error {
	ctx := context.Background()
	err := c.client.RPush(ctx, key, value).Err()
	if err != nil {
		return fmt.Errorf("failed to rpush to %s: %w", key, err)
	}
	return nil
}

// BLPop blocks and pops from the left end of a Redis list
func (c *Client) BLPop(ctx context.Context, timeout time.Duration, keys ...string) (string, error) {
	result, err := c.client.BLPop(ctx, timeout, keys...).Result()
	if err == redis.Nil {
		return "", nil // Timeout
	}
	if err != nil {
		return "", fmt.Errorf("failed to blpop: %w", err)
	}
	if len(result) < 2 {
		return "", fmt.Errorf("unexpected blpop result: %v", result)
	}
	return result[1], nil
}

// Publish publishes a message to a Redis channel
func (c *Client) Publish(channel, message string) error {
	ctx := context.Background()
	err := c.client.Publish(ctx, channel, message).Err()
	if err != nil {
		return fmt.Errorf("failed to publish to %s: %w", channel, err)
	}
	return nil
}