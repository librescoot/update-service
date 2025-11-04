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

// GetClient returns the underlying Redis client for direct access
func (c *Client) GetClient() *redis.Client {
	return c.client
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

// PushUpdateCommand pushes an update command to the scooter:update list
func (c *Client) PushUpdateCommand(command string) error {
	return c.client.LPush(c.ctx, "scooter:update", command).Err()
}

// GetOTAStatus gets the OTA status from Redis
func (c *Client) GetOTAStatus(otaHashKey string) (map[string]string, error) {
	return c.client.HGetAll(c.ctx, otaHashKey).Result()
}

// SetOTAStatus sets a field in the OTA status hash
func (c *Client) SetOTAStatus(otaHashKey, field, value string) error {
	return c.client.HSet(c.ctx, otaHashKey, field, value).Err()
}

// GetOTAField gets a specific field from the OTA status hash
func (c *Client) GetOTAField(otaHashKey, field string) (string, error) {
	return c.client.HGet(c.ctx, otaHashKey, field).Result()
}

// SubscribeToOTAStatus subscribes to the OTA status channel
// It returns a channel that will receive messages when the OTA status changes
func (c *Client) SubscribeToOTAStatus(channel string) (<-chan string, func(), error) {
	pubsub := c.client.Subscribe(c.ctx, channel)

	// Check if subscription was successful
	_, err := pubsub.Receive(c.ctx)
	if err != nil {
		pubsub.Close()
		return nil, nil, fmt.Errorf("failed to subscribe to channel %s: %w", channel, err)
	}

	// Create a string channel to convert redis.Message to string
	msgChan := make(chan string)

	// Start a goroutine to convert redis.Message to string
	go func() {
		defer close(msgChan)
		redisChan := pubsub.Channel()
		for {
			select {
			case <-c.ctx.Done():
				return
			case msg, ok := <-redisChan:
				if !ok {
					// Channel closed - Redis connection lost
					panic(fmt.Sprintf("Redis channel %s closed unexpectedly, exiting to allow systemd restart", channel))
				}
				msgChan <- msg.Payload
			}
		}
	}()

	// Return the string channel and a cleanup function
	return msgChan, func() { pubsub.Close() }, nil
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

// GetComponentVersion gets the installed version of a component from Redis
func (c *Client) GetComponentVersion(component string) (string, error) {
	versionHash := fmt.Sprintf("version:%s", component)
	versionID, err := c.client.HGet(c.ctx, versionHash, "version_id").Result()
	if err != nil {
		if err == redis.Nil {
			// Version not found, return empty string
			return "", nil
		}
		return "", fmt.Errorf("failed to get %s version: %w", component, err)
	}
	return versionID, nil
}

// GetVariantID gets the variant_id from the component's version hash
func (c *Client) GetVariantID(component string) (string, error) {
	versionHash := fmt.Sprintf("version:%s", component)
	variantID, err := c.client.HGet(c.ctx, versionHash, "variant_id").Result()
	if err != nil {
		if err == redis.Nil {
			// Variant ID not found - for backward compatibility, return component name
			return component, nil
		}
		return "", fmt.Errorf("failed to get variant_id for %s: %w", component, err)
	}
	return variantID, nil
}

// GetVehicleStateWithTimestamp gets vehicle state and last state change timestamp
func (c *Client) GetVehicleStateWithTimestamp(vehicleHashKey string) (string, time.Time, error) {
	result, err := c.client.HMGet(c.ctx, vehicleHashKey, "state", "state_timestamp").Result()
	if err != nil {
		return "", time.Time{}, err
	}

	state := ""
	if result[0] != nil {
		state = result[0].(string)
	}

	var timestamp time.Time
	if result[1] != nil {
		if ts, ok := result[1].(string); ok && ts != "" {
			if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
				timestamp = parsed
			}
		}
	}

	return state, timestamp, nil
}

// SubscribeToVehicleStateChanges subscribes to vehicle state changes
func (c *Client) SubscribeToVehicleStateChanges(channel string) (<-chan string, func(), error) {
	pubsub := c.client.Subscribe(c.ctx, channel)

	// Check if subscription was successful
	_, err := pubsub.Receive(c.ctx)
	if err != nil {
		pubsub.Close()
		return nil, nil, fmt.Errorf("failed to subscribe to vehicle state changes: %w", err)
	}

	// Create a string channel
	msgChan := make(chan string)

	// Start a goroutine to handle messages
	go func() {
		defer close(msgChan)
		redisChan := pubsub.Channel()
		for {
			select {
			case <-c.ctx.Done():
				return
			case msg, ok := <-redisChan:
				if !ok {
					// Channel closed - Redis connection lost
					panic(fmt.Sprintf("Redis channel %s closed unexpectedly, exiting to allow systemd restart", channel))
				}
				msgChan <- msg.Payload
			}
		}
	}()

	return msgChan, func() { pubsub.Close() }, nil
}

// TriggerReboot triggers a system reboot via Redis
func (c *Client) TriggerReboot() error {
	return c.client.LPush(c.ctx, "scooter:power", "reboot").Err()
}

// GetUpdateMethod gets the configured update method for a component from Redis settings
// Returns "full" by default if not configured
func (c *Client) GetUpdateMethod(component string) (string, error) {
	key := fmt.Sprintf("updates.%s.method", component)
	method, err := c.client.HGet(c.ctx, "settings", key).Result()
	if err != nil {
		if err == redis.Nil {
			// Default to full if not configured
			return "full", nil
		}
		return "", fmt.Errorf("failed to get update method for %s: %w", component, err)
	}

	// Validate the method
	if method != "delta" && method != "full" {
		return "full", nil // Default to full for invalid values
	}

	return method, nil
}

// SubscribeToDashboardReady subscribes to the dashboard ready channel
// and returns a channel that will receive a signal when the dashboard is ready
// The context can be used to cancel the subscription
func (c *Client) SubscribeToDashboardReady(ctx context.Context, channel string) <-chan struct{} {
	// Create a pubsub instance
	pubsub := c.client.Subscribe(ctx, channel)

	// Check if subscription was successful
	_, err := pubsub.Receive(ctx)
	if err != nil {
		pubsub.Close()
		return nil
	}

	// Create a signal channel
	readyChan := make(chan struct{})

	// Start a goroutine to monitor for the "ready" message
	go func() {
		defer pubsub.Close()
		defer close(readyChan)

		redisChan := pubsub.Channel()
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-redisChan:
				if !ok {
					// Channel closed - Redis connection lost
					panic(fmt.Sprintf("Redis channel %s closed unexpectedly, exiting to allow systemd restart", channel))
				}
				if msg.Payload == "ready" {
					readyChan <- struct{}{}
					return
				}
			}
		}
	}()

	return readyChan
}

// SubscribeToSettingsChanges subscribes to settings change notifications
// Returns a channel that receives the setting key that changed and a cleanup function
func (c *Client) SubscribeToSettingsChanges(channel string) (<-chan string, func(), error) {
	pubsub := c.client.Subscribe(c.ctx, channel)

	// Check if subscription was successful
	_, err := pubsub.Receive(c.ctx)
	if err != nil {
		pubsub.Close()
		return nil, nil, fmt.Errorf("failed to subscribe to settings changes on channel %s: %w", channel, err)
	}

	// Create a string channel for setting changes
	msgChan := make(chan string)

	// Start a goroutine to handle messages
	go func() {
		defer close(msgChan)
		redisChan := pubsub.Channel()
		for {
			select {
			case <-c.ctx.Done():
				return
			case msg, ok := <-redisChan:
				if !ok {
					// Channel closed - Redis connection lost
					panic(fmt.Sprintf("Redis channel %s closed unexpectedly, exiting to allow systemd restart", channel))
				}
				msgChan <- msg.Payload
			}
		}
	}()

	return msgChan, func() { pubsub.Close() }, nil
}

// HGet gets a field value from a Redis hash
func (c *Client) HGet(key, field string) (string, error) {
	val, err := c.client.HGet(c.ctx, key, field).Result()
	if err != nil {
		if err == redis.Nil {
			return "", fmt.Errorf("field not found")
		}
		return "", err
	}
	return val, nil
}

// SetLastUpdateCheckTime stores the timestamp of the last update check for a component
func (c *Client) SetLastUpdateCheckTime(component string, timestamp time.Time) error {
	key := fmt.Sprintf("updates.%s.last-check-time", component)
	return c.client.HSet(c.ctx, "settings", key, timestamp.Format(time.RFC3339)).Err()
}

// GetLastUpdateCheckTime retrieves the timestamp of the last update check for a component
// Returns zero time if not found
func (c *Client) GetLastUpdateCheckTime(component string) (time.Time, error) {
	key := fmt.Sprintf("updates.%s.last-check-time", component)
	timeStr, err := c.client.HGet(c.ctx, "settings", key).Result()
	if err != nil {
		if err == redis.Nil {
			// Not found, return zero time
			return time.Time{}, nil
		}
		return time.Time{}, fmt.Errorf("failed to get last check time for %s: %w", component, err)
	}

	timestamp, err := time.Parse(time.RFC3339, timeStr)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to parse last check time for %s: %w", component, err)
	}

	return timestamp, nil
}
