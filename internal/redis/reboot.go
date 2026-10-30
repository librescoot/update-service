package redis

import (
	"context"
	"fmt"
	"time"
)

type MDBRebootState struct {
	VehicleState   string
	DashboardPower string
	DBCUpdating    string
	DBCStatus      string
	DBCBootStatus  string
	Owner          string
	StateSince     time.Time
}

// Read one snapshot: completion and dashboard shutdown are separate writes.
func (c *Client) GetMDBRebootState(ctx context.Context) (MDBRebootState, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	values, err := c.client.Raw().Eval(ctx, `
return {
  redis.call('HGET', KEYS[1], 'state') or '',
  redis.call('HGET', KEYS[1], 'dashboard:power') or '',
  redis.call('HGET', KEYS[1], 'dbc-updating') or '',
  redis.call('HGET', KEYS[2], 'status:dbc') or '',
  redis.call('HGET', KEYS[2], 'status:dbc-boot') or '',
  redis.call('HGET', KEYS[2], 'reboot-owner:mdb') or '',
  redis.call('HGET', KEYS[1], 'state:timestamp') or ''
}`, []string{"vehicle", "ota"}).StringSlice()
	if err != nil {
		return MDBRebootState{}, err
	}
	if len(values) != 7 {
		return MDBRebootState{}, fmt.Errorf("unexpected MDB reboot snapshot length: %d", len(values))
	}
	return MDBRebootState{
		VehicleState: values[0], DashboardPower: values[1], DBCUpdating: values[2],
		DBCStatus: values[3], DBCBootStatus: values[4], Owner: values[5],
		StateSince: parseVehicleTimestamp(values[6]),
	}, nil
}
