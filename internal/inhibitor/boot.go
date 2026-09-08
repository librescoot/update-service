package inhibitor

import (
	"context"
	"errors"
	"fmt"

	ipc "github.com/librescoot/redis-ipc"
)

const BootBusyServicesHash = "power-manager:busy-services"

func bootInstallReason(component, requestID string) string {
	return fmt.Sprintf("installing boot update for %s (%s)", component, requestID)
}

func BootInstallAcknowledgementField(component, requestID string) string {
	return "update-service " + bootInstallReason(component, requestID) + " power-state-change"
}

// Boot writes have no A/B fallback. A delay must never expire mid-write.
// The ID stays stable for crash cleanup; the reason identifies this acquisition
// in pm-service's existing processed-inhibitor publication.
func (c *Client) AddBootInstallInhibit(component, requestID string) error {
	return c.AddInhibit("install:"+component+"-boot", "update-service", "power-state-change",
		bootInstallReason(component, requestID), TypeBlock, 0)
}

func (c *Client) RemoveBootInstallInhibit(component string) error {
	return c.RemoveInhibit("install:" + component + "-boot")
}

// Read both fields in one transaction, not a state observation made before an
// acknowledgement. Only running is accepted: pending/imminent states can be
// waiting for inhibitors, but we do not risk joining a transition already begun.
func (c *Client) BootInstallObserved(ctx context.Context, component, requestID string) (bool, error) {
	pipeline := c.client.Raw().TxPipeline()
	state := pipeline.HGet(ctx, "power-manager", "state")
	observed := pipeline.HGet(ctx, BootBusyServicesHash, BootInstallAcknowledgementField(component, requestID))
	_, err := pipeline.Exec(ctx)
	if err != nil && !errors.Is(err, ipc.ErrNil) {
		return false, err
	}
	if err := state.Err(); err != nil {
		return false, fmt.Errorf("read power-manager state: %w", err)
	}
	if state.Val() != "running" {
		return false, fmt.Errorf("unsafe power-manager state %q", state.Val())
	}
	if errors.Is(observed.Err(), ipc.ErrNil) {
		return false, nil
	}
	if err := observed.Err(); err != nil {
		return false, err
	}
	if observed.Val() != string(TypeBlock) {
		return false, fmt.Errorf("boot inhibitor acknowledged as %q, not block", observed.Val())
	}
	return true, ctx.Err()
}
