// Package power requests CPU governor changes from pm-service via the
// scooter:governor list. Power inhibits live in internal/inhibitor.
package power

import (
	"fmt"
	"log"

	ipc "github.com/librescoot/redis-ipc"
)

// PowerGovernorListKey is the pm-service command list for governor changes.
const PowerGovernorListKey = "scooter:governor"

// Client requests CPU governor changes from pm-service.
type Client struct {
	client *ipc.Client
	logger *log.Logger
}

// New creates a power manager client on the caller's shared redis-ipc
// client. It does not own the connection.
func New(client *ipc.Client, logger *log.Logger) *Client {
	return &Client{
		client: client,
		logger: logger,
	}
}

// RequestOndemandGovernor asks pm-service to switch to the ondemand governor
// so downloads and delta application don't run at powersave clocks.
func (c *Client) RequestOndemandGovernor() error {
	c.logger.Printf("Requesting CPU governor change to: ondemand")

	if _, err := c.client.LPush(PowerGovernorListKey, "ondemand"); err != nil {
		return fmt.Errorf("failed to request governor change: %w", err)
	}

	return nil
}
