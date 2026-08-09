package inhibitor

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	ipc "github.com/librescoot/redis-ipc"
)

const (
	// Redis keys for power inhibits
	InhibitHashKey = "power:inhibits"
	InhibitChannel = "power:inhibits"
)

// InhibitType represents the type of power inhibit
type InhibitType string

const (
	TypeBlock       InhibitType = "block"        // Block power state changes completely
	TypeDelay       InhibitType = "delay"        // Delay power state changes for a specified duration
	TypeSuspendOnly InhibitType = "suspend-only" // Blocks suspend but not hibernate, poweroff or reboot
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
	client *ipc.Client
	logger *log.Logger
}

// New creates a power inhibitor client on the caller's shared redis-ipc
// client. It does not own the connection.
func New(client *ipc.Client, logger *log.Logger) *Client {
	return &Client{
		client: client,
		logger: logger,
	}
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

	// HSET + PUBLISH stay in one pipeline so a subscriber never sees the
	// notification before the hash entry exists.
	_, err = c.client.NewTxGroup().
		Add("HSET", InhibitHashKey, id, string(data)).
		Add("PUBLISH", InhibitChannel, fmt.Sprintf("add:%s", id)).
		Exec()
	if err != nil {
		return fmt.Errorf("failed to add power inhibit: %w", err)
	}

	return nil
}

// RemoveInhibit removes a power inhibit
func (c *Client) RemoveInhibit(id string) error {
	c.logger.Printf("Removing power inhibit: id=%s", id)

	_, err := c.client.NewTxGroup().
		Add("HDEL", InhibitHashKey, id).
		Add("PUBLISH", InhibitChannel, fmt.Sprintf("remove:%s", id)).
		Exec()
	if err != nil {
		return fmt.Errorf("failed to remove power inhibit: %w", err)
	}

	return nil
}

// AddDownloadInhibit adds a download inhibit that delays power state changes
// for up to 15 seconds while an update is downloading
func (c *Client) AddDownloadInhibit(componentID string) error {
	id := fmt.Sprintf("download:%s", componentID)
	who := "update-service"
	what := "power-state-change"
	why := fmt.Sprintf("downloading update for %s", componentID)
	return c.AddInhibit(id, who, what, why, TypeDelay, 15*time.Second)
}

// RemoveDownloadInhibit removes a download inhibit
func (c *Client) RemoveDownloadInhibit(componentID string) error {
	id := fmt.Sprintf("download:%s", componentID)
	return c.RemoveInhibit(id)
}

// AddDownloadSuspendInhibit holds off idle suspend while a download is running.
//
// This has to be suspend-only, not delay: pm-service honours only block and
// suspend-only, so a delay inhibit would let the MDB suspend about a minute
// into stand-by and take the modem with it. It must not be block either, since
// a download has no business standing in the way of a hibernate.
//
// Safe only because the download itself is bounded. An unbounded hold by a
// transfer that never finishes is the AUX drain this whole mechanism exists to
// prevent, so every exit path must remove it.
func (c *Client) AddDownloadSuspendInhibit(componentID string) error {
	id := fmt.Sprintf("download-transfer:%s", componentID)
	return c.AddInhibit(id, "update-service", "power-state-change",
		fmt.Sprintf("downloading update for %s", componentID), TypeSuspendOnly, 0)
}

// RemoveDownloadSuspendInhibit removes the suspend hold taken for a transfer.
func (c *Client) RemoveDownloadSuspendInhibit(componentID string) error {
	return c.RemoveInhibit(fmt.Sprintf("download-transfer:%s", componentID))
}

// AddPreparingInhibit adds a preparing inhibit that delays power state changes
// for up to 30 seconds while delta application is in progress
func (c *Client) AddPreparingInhibit(componentID string) error {
	id := fmt.Sprintf("preparing:%s", componentID)
	who := "update-service"
	what := "power-state-change"
	why := fmt.Sprintf("preparing update for %s", componentID)
	return c.AddInhibit(id, who, what, why, TypeDelay, 30*time.Second)
}

// RemovePreparingInhibit removes a preparing inhibit
func (c *Client) RemovePreparingInhibit(componentID string) error {
	id := fmt.Sprintf("preparing:%s", componentID)
	return c.RemoveInhibit(id)
}

// AddInstallInhibit adds an install inhibit that delays power state changes
// for up to 60 seconds while an update is being installed
func (c *Client) AddInstallInhibit(componentID string) error {
	id := fmt.Sprintf("install:%s", componentID)
	who := "update-service"
	what := "power-state-change"
	why := fmt.Sprintf("installing update for %s", componentID)
	return c.AddInhibit(id, who, what, why, TypeDelay, 60*time.Second)
}

// RemoveInstallInhibit removes an install inhibit
func (c *Client) RemoveInstallInhibit(componentID string) error {
	id := fmt.Sprintf("install:%s", componentID)
	return c.RemoveInhibit(id)
}
