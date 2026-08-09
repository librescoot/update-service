package status

import (
	"context"
	"log"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	ipc "github.com/librescoot/redis-ipc"
)

func newTestReporter(t *testing.T) (*Reporter, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client, err := ipc.New(ipc.WithURL(mr.Addr()), ipc.WithCodec(ipc.StringCodec{}))
	if err != nil {
		t.Fatalf("connecting test client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return NewReporter(client, "mdb", log.New(os.Stdout, "test: ", 0)), mr
}

func TestSetAborted_PreservesProgressAndRecordsReason(t *testing.T) {
	r, mr := newTestReporter(t)
	ctx := context.Background()

	if err := r.SetDownloading(ctx, "v1.2.3", "full"); err != nil {
		t.Fatal(err)
	}
	// Seed progress directly rather than through SetDownloadProgress: that
	// call is async by design (see the comment on Reporter), so asserting
	// on it immediately after would race the write it queues.
	mr.HSet("ota", "download-bytes:mdb", "5000000")
	mr.HSet("ota", "download-total:mdb", "10000000")

	retryAfter := time.Date(2026, 8, 9, 18, 0, 0, 0, time.UTC)
	if err := r.SetAborted(ctx, "stalled", retryAfter); err != nil {
		t.Fatal(err)
	}

	if got := mr.HGet("ota", "status:mdb"); got != string(StatusIdle) {
		t.Errorf("status:mdb = %q, want idle", got)
	}
	// The whole point of a dedicated transition: SetIdle would have wiped these.
	if got := mr.HGet("ota", "download-bytes:mdb"); got != "5000000" {
		t.Errorf("download-bytes:mdb = %q, want the preserved progress", got)
	}
	if got := mr.HGet("ota", "download-total:mdb"); got != "10000000" {
		t.Errorf("download-total:mdb = %q, want the preserved total", got)
	}
	if got := mr.HGet("ota", "download-abort-reason:mdb"); got != "stalled" {
		t.Errorf("download-abort-reason:mdb = %q, want stalled", got)
	}
	wantRetry := strconv.FormatInt(retryAfter.Unix(), 10)
	if got := mr.HGet("ota", "download-retry-after:mdb"); got != wantRetry {
		t.Errorf("download-retry-after:mdb = %q, want %q", got, wantRetry)
	}
	if got := mr.HGet("ota", "error:mdb"); got != "" {
		t.Errorf("error:mdb = %q, want cleared", got)
	}
}

func TestSetAborted_ZeroRetryAfterClearsTheField(t *testing.T) {
	r, mr := newTestReporter(t)
	ctx := context.Background()
	if err := r.SetAborted(ctx, "stalled", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got := mr.HGet("ota", "download-retry-after:mdb"); got != "" {
		t.Errorf("download-retry-after:mdb = %q, want empty when no backoff applies", got)
	}
}

func TestInitialize_LeavesAbortFieldsIntact(t *testing.T) {
	r, mr := newTestReporter(t)
	ctx := context.Background()

	retryAfter := time.Date(2026, 8, 9, 18, 0, 0, 0, time.UTC)
	if err := r.SetAborted(ctx, "budget-exceeded", retryAfter); err != nil {
		t.Fatal(err)
	}
	// Initialize runs on every service start, which for the DBC is every
	// dashboard power-on. Clearing these here would wipe the orchestrator's
	// gate on every ride.
	if err := r.Initialize(ctx, "full"); err != nil {
		t.Fatal(err)
	}

	if got := mr.HGet("ota", "download-abort-reason:mdb"); got != "budget-exceeded" {
		t.Errorf("download-abort-reason:mdb = %q, want it to survive Initialize", got)
	}
	wantRetry := strconv.FormatInt(retryAfter.Unix(), 10)
	if got := mr.HGet("ota", "download-retry-after:mdb"); got != wantRetry {
		t.Errorf("download-retry-after:mdb = %q, want it to survive Initialize", got)
	}
}

func TestSetDownloading_ClearsAbortFields(t *testing.T) {
	r, mr := newTestReporter(t)
	ctx := context.Background()
	if err := r.SetAborted(ctx, "stalled", time.Date(2026, 8, 9, 18, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if err := r.SetDownloading(ctx, "v1.2.3", "full"); err != nil {
		t.Fatal(err)
	}
	if got := mr.HGet("ota", "download-abort-reason:mdb"); got != "" {
		t.Errorf("download-abort-reason:mdb = %q, want cleared by a fresh attempt", got)
	}
	if got := mr.HGet("ota", "download-retry-after:mdb"); got != "" {
		t.Errorf("download-retry-after:mdb = %q, want cleared by a fresh attempt", got)
	}
}

func TestSetHeartbeat_WritesUnixSeconds(t *testing.T) {
	r, mr := newTestReporter(t)
	beat := time.Unix(1786298400, 0)
	if err := r.SetHeartbeat(context.Background(), beat); err != nil {
		t.Fatal(err)
	}
	// SetHeartbeat is async by design (see its doc comment), so give the
	// fire-and-forget write a moment to land before asserting on it.
	deadline := time.Now().Add(time.Second)
	var got string
	for time.Now().Before(deadline) {
		got = mr.HGet("ota", "heartbeat:mdb")
		if got == "1786298400" {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Errorf("heartbeat:mdb = %q, want 1786298400", got)
}
