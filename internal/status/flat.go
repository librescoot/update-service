package status

import (
	"context"
	"fmt"
	"log"

	ipc "github.com/librescoot/redis-ipc"
)

// FlatFor combines the MDB and DBC local statuses into the flat `status` /
// `update-type` pair mirrored on the ota hash, for consumers that only
// understand the stock non-namespaced convention. The pair reflects
// whichever component is least advanced through an update: a consumer
// asking "is this scooter mid-update" should not see the pair clear while
// one board is still pulling bytes, so it only clears once both components
// are idle, in error, absent, or holding a status this mapping does not
// recognize. error maps to empty, consistent with flatFor.
func FlatFor(mdb, dbc Status) (flatStatus, updateType string) {
	s := mdb
	if flatStageRank(dbc) < flatStageRank(s) {
		s = dbc
	}
	return flatFor(s)
}

// flatFor returns the flat (status, update-type) values for a single local
// state. Empty strings clear the field, which stock-style consumers treat
// as undefined / not-updating.
func flatFor(s Status) (flatStatus, updateType string) {
	switch s {
	case StatusDownloading:
		return "downloading-updates", "blocking"
	case StatusPreparing, StatusInstalling:
		return "installing-updates", "blocking"
	case StatusPendingReboot:
		return "installation-complete-waiting-reboot", "blocking"
	default:
		return "", ""
	}
}

// flatStageRank orders a status by how early it sits in an update, lower
// ranking earlier. Statuses flatFor does not treat as busy (idle, error, an
// absent field, or a value this mapping does not recognize) all rank last,
// so none of them can outrank a genuinely busy status in FlatFor.
func flatStageRank(s Status) int {
	switch s {
	case StatusDownloading:
		return 0
	case StatusPreparing, StatusInstalling:
		return 1
	case StatusPendingReboot:
		return 2
	default:
		return 3
	}
}

// FlatMirror writes the flat `status` / `update-type` pair to the ota hash.
// Owned by the MDB instance only: it is the side that watches both
// status:mdb and status:dbc, and a second writer would race it on the same
// fields.
type FlatMirror struct {
	pub    *ipc.HashPublisher
	logger *log.Logger
}

// NewFlatMirror creates a FlatMirror writing to the ota hash.
func NewFlatMirror(client *ipc.Client, logger *log.Logger) *FlatMirror {
	return &FlatMirror{
		pub:    client.NewHashPublisher("ota"),
		logger: logger,
	}
}

// Write computes the flat pair from both component statuses and publishes
// it in a single SetMany call, so no consumer sees status and update-type
// disagree even momentarily.
func (m *FlatMirror) Write(ctx context.Context, mdb, dbc Status) error {
	flatStatus, updateType := FlatFor(mdb, dbc)
	if err := m.pub.SetMany(map[string]any{
		"status":      flatStatus,
		"update-type": updateType,
	}, ipc.Sync()); err != nil {
		return fmt.Errorf("write flat status: %w", err)
	}
	m.logger.Printf("Flat status: mdb=%s dbc=%s -> status=%q update-type=%q", mdb, dbc, flatStatus, updateType)
	return nil
}
