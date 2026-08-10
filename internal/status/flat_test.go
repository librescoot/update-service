package status

import (
	"context"
	"log"
	"os"
	"testing"

	"github.com/alicebob/miniredis/v2"
	ipc "github.com/librescoot/redis-ipc"
)

func TestFlatFor(t *testing.T) {
	const unknown = Status("some-future-status")
	const absent = Status("")

	tests := []struct {
		name           string
		mdb, dbc       Status
		wantFlatStatus string
		wantUpdateType string
	}{
		{"both idle", StatusIdle, StatusIdle, "", ""},
		{"both absent", absent, absent, "", ""},

		{"mdb downloading, dbc idle", StatusDownloading, StatusIdle, "downloading-updates", "blocking"},
		{"dbc downloading, mdb idle", StatusIdle, StatusDownloading, "downloading-updates", "blocking"},
		{"both downloading", StatusDownloading, StatusDownloading, "downloading-updates", "blocking"},

		{"mdb preparing, dbc idle", StatusPreparing, StatusIdle, "installing-updates", "blocking"},
		{"dbc preparing, mdb idle", StatusIdle, StatusPreparing, "installing-updates", "blocking"},
		{"mdb installing, dbc idle", StatusInstalling, StatusIdle, "installing-updates", "blocking"},
		{"dbc installing, mdb idle", StatusIdle, StatusInstalling, "installing-updates", "blocking"},
		{"mdb preparing, dbc installing", StatusPreparing, StatusInstalling, "installing-updates", "blocking"},

		{"mdb pending-reboot, dbc idle", StatusPendingReboot, StatusIdle, "installation-complete-waiting-reboot", "blocking"},
		{"dbc pending-reboot, mdb idle", StatusIdle, StatusPendingReboot, "installation-complete-waiting-reboot", "blocking"},
		{"both pending-reboot", StatusPendingReboot, StatusPendingReboot, "installation-complete-waiting-reboot", "blocking"},

		// Earliest stage wins: a component further along must not mask one
		// that is still busy earlier in the pipeline.
		{"mdb pending-reboot, dbc downloading", StatusPendingReboot, StatusDownloading, "downloading-updates", "blocking"},
		{"mdb downloading, dbc pending-reboot", StatusDownloading, StatusPendingReboot, "downloading-updates", "blocking"},
		{"mdb pending-reboot, dbc installing", StatusPendingReboot, StatusInstalling, "installing-updates", "blocking"},
		{"mdb installing, dbc pending-reboot", StatusInstalling, StatusPendingReboot, "installing-updates", "blocking"},

		// error, absent and unrecognized values are all not-busy.
		{"mdb error, dbc idle", StatusError, StatusIdle, "", ""},
		{"mdb idle, dbc error", StatusIdle, StatusError, "", ""},
		{"both error", StatusError, StatusError, "", ""},
		{"mdb error, dbc downloading", StatusError, StatusDownloading, "downloading-updates", "blocking"},
		{"mdb downloading, dbc error", StatusDownloading, StatusError, "downloading-updates", "blocking"},

		{"mdb absent, dbc idle", absent, StatusIdle, "", ""},
		{"mdb idle, dbc absent", StatusIdle, absent, "", ""},
		{"mdb absent, dbc downloading", absent, StatusDownloading, "downloading-updates", "blocking"},

		{"mdb unknown, dbc idle", unknown, StatusIdle, "", ""},
		{"mdb idle, dbc unknown", StatusIdle, unknown, "", ""},
		{"both unknown", unknown, unknown, "", ""},
		{"mdb unknown, dbc downloading", unknown, StatusDownloading, "downloading-updates", "blocking"},
		{"mdb unknown, dbc pending-reboot", unknown, StatusPendingReboot, "installation-complete-waiting-reboot", "blocking"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotFlatStatus, gotUpdateType := FlatFor(tc.mdb, tc.dbc)
			if gotFlatStatus != tc.wantFlatStatus || gotUpdateType != tc.wantUpdateType {
				t.Errorf("FlatFor(%q, %q) = (%q, %q), want (%q, %q)",
					tc.mdb, tc.dbc, gotFlatStatus, gotUpdateType, tc.wantFlatStatus, tc.wantUpdateType)
			}
		})
	}
}

func TestFlatMirror_Write(t *testing.T) {
	mr := miniredis.RunT(t)
	client, err := ipc.New(ipc.WithURL(mr.Addr()), ipc.WithCodec(ipc.StringCodec{}))
	if err != nil {
		t.Fatalf("connecting test client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	m := NewFlatMirror(client, log.New(os.Stdout, "test: ", 0))
	ctx := context.Background()

	if err := m.Write(ctx, StatusDownloading, StatusIdle); err != nil {
		t.Fatal(err)
	}
	if got := mr.HGet("ota", "status"); got != "downloading-updates" {
		t.Errorf("status = %q, want downloading-updates", got)
	}
	if got := mr.HGet("ota", "update-type"); got != "blocking" {
		t.Errorf("update-type = %q, want blocking", got)
	}

	if err := m.Write(ctx, StatusIdle, StatusIdle); err != nil {
		t.Fatal(err)
	}
	if got := mr.HGet("ota", "status"); got != "" {
		t.Errorf("status = %q, want cleared once both sides are idle", got)
	}
	if got := mr.HGet("ota", "update-type"); got != "" {
		t.Errorf("update-type = %q, want cleared once both sides are idle", got)
	}
}
