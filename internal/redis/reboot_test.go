package redis

import (
	"context"
	"testing"
	"time"
)

func TestMDBRebootSnapshot(t *testing.T) {
	client, mr := newTestClient(t)
	mr.HSet("vehicle", "state", "stand-by", "dashboard:power", "off", "dbc-updating", "false", "state:timestamp", "2026-10-30T16:00:00Z")
	mr.HSet("ota", "status:dbc", "pending-reboot", "status:dbc-boot", "idle", "reboot-owner:mdb", "ums")
	got, err := client.GetMDBRebootState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := MDBRebootState{"stand-by", "off", "false", "pending-reboot", "idle", "ums", time.Date(2026, 10, 30, 16, 0, 0, 0, time.UTC)}
	if got != want {
		t.Fatalf("snapshot = %+v, want %+v", got, want)
	}
}

func TestMDBRebootSnapshotMissingAndFailedReads(t *testing.T) {
	client, mr := newTestClient(t)
	got, err := client.GetMDBRebootState(context.Background())
	if err != nil || got != (MDBRebootState{}) {
		t.Fatalf("missing snapshot = %+v, %v", got, err)
	}
	mr.Set("vehicle", "wrong-type")
	if _, err := client.GetMDBRebootState(context.Background()); err == nil {
		t.Fatal("wrong hash type must fail")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.GetMDBRebootState(ctx); err == nil {
		t.Fatal("canceled read must fail")
	}
}
