package redis

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	ipc "github.com/librescoot/redis-ipc"
)

// Client represents a Redis IPC client for the update service
type Client struct {
	client *ipc.Client
}

// New creates a new Redis client using redis-ipc
func New(redisAddr string) (*Client, error) {
	// Parse address (format: "host:port" or "host")
	client, err := ipc.New(
		ipc.WithURL(redisAddr),
		ipc.WithCodec(ipc.StringCodec{}), // Use plain strings, not JSON
	)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	return &Client{
		client: client,
	}, nil
}

// Close closes the Redis client
func (c *Client) Close() error {
	return c.client.Close()
}

// GetClient returns the underlying redis-ipc client for advanced operations
func (c *Client) GetClient() *ipc.Client {
	return c.client
}

// SetVehicleState sets the vehicle state in Redis
func (c *Client) SetVehicleState(vehicleHashKey, state string) error {
	return c.client.HSet(vehicleHashKey, "state", state)
}

// GetVehicleState gets the vehicle state from Redis
func (c *Client) GetVehicleState(vehicleHashKey string) (string, error) {
	return c.client.HGet(vehicleHashKey, "state")
}

// PushUpdateURL pushes an update URL to the specified Redis key
func (c *Client) PushUpdateURL(updateKey, url string) error {
	_, err := c.client.LPush(updateKey, url)
	return err
}

// PushUpdateCommand pushes an update command to the scooter:update list
// Used for lifecycle commands (start-dbc, complete-dbc) that vehicle-service needs
func (c *Client) PushUpdateCommand(command string) error {
	return ipc.SendRequest(c.client, "scooter:update", command)
}

// PushUpdateCommandToComponent pushes an update command to a component-specific channel
// Used for check commands (check-now) that only the specific update-service should handle
func (c *Client) PushUpdateCommandToComponent(component, command string) error {
	channel := fmt.Sprintf("scooter:update:%s", component)
	return ipc.SendRequest(c.client, channel, command)
}

// GetOTAStatus gets the OTA status from Redis
func (c *Client) GetOTAStatus(otaHashKey string) (map[string]string, error) {
	return c.client.HGetAll(otaHashKey)
}

// SetOTAStatus sets a field in the OTA status hash
func (c *Client) SetOTAStatus(otaHashKey, field, value string) error {
	return c.client.HSet(otaHashKey, field, value)
}

// GetOTAField gets a specific field from the OTA status hash
func (c *Client) GetOTAField(otaHashKey, field string) (string, error) {
	return c.client.HGet(otaHashKey, field)
}

// WaitForOTAStatus waits for the OTA status to match the expected status
// It polls the OTA status hash at the specified interval until the status matches
// or the context is cancelled
func (c *Client) WaitForOTAStatus(otaHashKey, statusField, expectedStatus string, pollInterval time.Duration) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	ctx := c.client.Context()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			status, err := c.client.HGet(otaHashKey, statusField)
			if err != nil {
				// Field doesn't exist yet, continue polling
				continue
			}

			if status == expectedStatus {
				return nil
			}
		}
	}
}

type DBCStateFacts struct {
	RunningVersion string
	TargetVersion  string
	Status         string
	Origin         string
}

func (c *Client) SetComponentRunningVersion(component, version string) error {
	return c.client.HSet(fmt.Sprintf("version:%s", component), "version_id", version)
}

// GetComponentVersion gets the running version of a component from Redis.
func (c *Client) GetComponentVersion(component string) (string, error) {
	versionHash := fmt.Sprintf("version:%s", component)
	versionID, err := c.client.HGet(versionHash, "version_id")
	if err != nil {
		// Version not found, return empty string
		return "", nil
	}
	return versionID, nil
}

// GetVariantID gets the variant_id from the component's version hash
func (c *Client) GetVariantID(component string) (string, error) {
	versionHash := fmt.Sprintf("version:%s", component)
	variantID, err := c.client.HGet(versionHash, "variant_id")
	if err != nil {
		// Variant ID not found - for backward compatibility, return component name
		return component, nil
	}
	return variantID, nil
}

// GetVehicleStateWithTimestamp gets vehicle state and last state change timestamp
func (c *Client) GetVehicleStateWithTimestamp(vehicleHashKey string) (string, time.Time, error) {
	// Use the raw client for HMGET since redis-ipc doesn't have a direct equivalent
	ctx := c.client.Context()
	result, err := c.client.Raw().HMGet(ctx, vehicleHashKey, "state", "state:timestamp").Result()
	if err != nil {
		return "", time.Time{}, err
	}

	state := ""
	if result[0] != nil {
		state = result[0].(string)
	}

	var timestamp time.Time
	if result[1] != nil {
		if ts, ok := result[1].(string); ok {
			timestamp = parseVehicleTimestamp(ts)
		}
	}

	return state, timestamp, nil
}

// parseVehicleTimestamp parses the vehicle state:timestamp field, which is
// written by vehicle-service via redis-ipc SetWithTimestamp. The encoding
// depends on the redis-ipc version on the writer: RFC3339 (>= v0.11) or Unix
// milliseconds (<= v0.10). Handle both so the standby timer anchors on the real
// state-entry time on either format; returns the zero time when unparseable so
// callers fall back to time.Now().
func parseVehicleTimestamp(ts string) time.Time {
	if ts == "" {
		return time.Time{}
	}
	if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
		return parsed
	}
	if ms, err := strconv.ParseInt(ts, 10, 64); err == nil && ms > 0 {
		return time.UnixMilli(ms)
	}
	return time.Time{}
}

// NewVehicleWatcher creates a HashWatcher for the vehicle hash
func (c *Client) NewVehicleWatcher(vehicleHashKey string) *ipc.HashWatcher {
	return c.client.NewHashWatcher(vehicleHashKey)
}

// NewOTAWatcher creates a HashWatcher for the OTA hash
func (c *Client) NewOTAWatcher(otaHashKey string) *ipc.HashWatcher {
	return c.client.NewHashWatcher(otaHashKey)
}

// NewSettingsWatcher creates a HashWatcher for the settings hash
func (c *Client) NewSettingsWatcher() *ipc.HashWatcher {
	return c.client.NewHashWatcher("settings")
}

// TriggerReboot triggers a system reboot via Redis
func (c *Client) TriggerReboot() error {
	return ipc.SendRequest(c.client, "scooter:power", "reboot")
}

// GetUpdateMethod gets the configured update method for a component from Redis settings
// Returns "delta" by default if not configured
func (c *Client) GetUpdateMethod(component string) (string, error) {
	key := fmt.Sprintf("updates.%s.method", component)
	method, err := c.client.HGet("settings", key)
	if errors.Is(err, ipc.ErrNil) {
		return "delta", nil
	}
	if err != nil {
		return "", fmt.Errorf("read update method: %w", err)
	}

	// Validate the method
	if method != "delta" && method != "full" {
		return "delta", nil // Default to delta for invalid values
	}

	return method, nil
}

// GetUpdateChannel distinguishes an absent override from a failed read. A
// failed read must not make a channel-switch check silently use its old channel.
func (c *Client) GetUpdateChannel(component string) (string, error) {
	value, err := c.client.HGet("settings", "updates."+component+".channel")
	if errors.Is(err, ipc.ErrNil) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read update channel: %w", err)
	}
	return value, nil
}

// HGet gets a field value from a Redis hash
func (c *Client) HGet(key, field string) (string, error) {
	val, err := c.client.HGet(key, field)
	if err != nil {
		return "", fmt.Errorf("field not found")
	}
	return val, nil
}

// SetLastUpdateCheckTime stores the timestamp of the last update check for a component
func (c *Client) SetLastUpdateCheckTime(component string, timestamp time.Time) error {
	key := fmt.Sprintf("updates.%s.last-check-time", component)
	return c.client.HSet("settings", key, timestamp.Format(time.RFC3339))
}

// GetLastUpdateCheckTime retrieves the timestamp of the last update check for a component
// Returns zero time if not found
func (c *Client) GetLastUpdateCheckTime(component string) (time.Time, error) {
	key := fmt.Sprintf("updates.%s.last-check-time", component)
	timeStr, err := c.client.HGet("settings", key)
	if err != nil {
		// Not found, return zero time
		return time.Time{}, nil
	}

	timestamp, err := time.Parse(time.RFC3339, timeStr)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to parse last check time for %s: %w", component, err)
	}

	return timestamp, nil
}

// HandleUpdateCommands sets up a handler for update commands on a component-specific queue
func (c *Client) HandleUpdateCommands(component string, handler func(string) error) *ipc.QueueHandler[string] {
	channel := fmt.Sprintf("scooter:update:%s", component)
	return ipc.HandleRequests(c.client, channel, handler)
}

func (c *Client) GetDBCStateFacts() (DBCStateFacts, error) {
	running, err := c.client.HGet("version:dbc", "version_id")
	if err != nil {
		running = ""
	}
	ota, err := c.client.HGetAll("ota")
	if err != nil {
		return DBCStateFacts{}, err
	}
	return DBCStateFacts{
		RunningVersion: running,
		TargetVersion:  ota["update-version:dbc"],
		Status:         ota["status:dbc"],
		Origin:         ota["state-origin:dbc"],
	}, nil
}

// RestoreDBCStateFacts conditionally restores one persisted snapshot after
// Redis loss. The Lua transaction prevents an older cache from overwriting a
// DBC that starts publishing live state concurrently.
func (c *Client) RestoreDBCStateFacts(facts DBCStateFacts) (bool, error) {
	const script = `
if redis.call('HGET', KEYS[1], 'version_id') then
  return 0
end
if redis.call('HGET', KEYS[2], 'state-origin:dbc') == 'live' then
  return 0
end
if redis.call('HGET', KEYS[2], 'status:dbc') then
  return 0
end
redis.call('HSET', KEYS[1], 'version_id', ARGV[1])
redis.call('HSET', KEYS[2], 'status:dbc', ARGV[2], 'update-version:dbc', ARGV[3], 'state-origin:dbc', 'cached')
return 1`
	result, err := c.client.Do("EVAL", script, 2, "version:dbc", "ota",
		facts.RunningVersion, facts.Status, facts.TargetVersion)
	if err != nil {
		return false, err
	}
	return fmt.Sprint(result) == "1", nil
}

func (c *Client) SetDBCStateOrigin(origin string) error {
	return c.client.HSet("ota", "state-origin:dbc", origin)
}

func (c *Client) GetRebootOwner(component string) (string, error) {
	ota, err := c.client.HGetAll("ota")
	if err != nil {
		return "", err
	}
	return ota[fmt.Sprintf("reboot-owner:%s", component)], nil
}

func (c *Client) GetPendingLifecycleCompletion(component string) (string, error) {
	value, err := c.client.HGet("ota", fmt.Sprintf("completion-pending:%s", component))
	if err != nil {
		return "", nil
	}
	return value, nil
}

func (c *Client) SetPendingLifecycleCompletion(component, value string) error {
	return c.client.HSet("ota", fmt.Sprintf("completion-pending:%s", component), value)
}

// GetTargetVersion gets the target update version for a component from the OTA hash
// This is set during download/install to track what version we're updating to
func (c *Client) GetTargetVersion(component string) (string, error) {
	key := fmt.Sprintf("update-version:%s", component)
	version, err := c.client.HGet("ota", key)
	if err != nil {
		return "", nil // Not set
	}
	return version, nil
}

// SendHardwareCommand sends a command to vehicle-service via the scooter:hardware queue.
// Used for dashboard:on, dashboard:off, etc.
func (c *Client) SendHardwareCommand(command string) error {
	return ipc.SendRequest(c.client, "scooter:hardware", command)
}

// GetDBCOTAStatus reads the DBC's OTA status from the shared ota hash.
func (c *Client) GetDBCOTAStatus() (string, error) {
	return c.client.HGet("ota", "status:dbc")
}

// GetOrchestrateDBC reads the orchestrate-dbc setting from Redis.
// Returns the string value ("true"/"false") and whether it was explicitly set.
func (c *Client) GetOrchestrateDBC() (string, bool) {
	val, err := c.client.HGet("settings", "updates.mdb.orchestrate-dbc")
	if err != nil || val == "" {
		return "", false
	}
	return val, true
}

// GetDBCChannel reads the DBC update channel from Redis settings.
// Returns empty string if not set.
func (c *Client) GetDBCChannel() string {
	val, err := c.client.HGet("settings", "updates.dbc.channel")
	if err != nil {
		return ""
	}
	return val
}
