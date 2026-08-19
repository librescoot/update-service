package status

import (
	"context"
	"log"
	"os"
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

	if err := r.SetAborted(ctx, "stalled", 4); err != nil {
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
	if got := mr.HGet("ota", "download-skip-checks:mdb"); got != "4" {
		t.Errorf("download-skip-checks:mdb = %q, want 4", got)
	}
	if got := mr.HGet("ota", "error:mdb"); got != "" {
		t.Errorf("error:mdb = %q, want cleared", got)
	}
}

func TestSetAborted_ZeroSkipChecksClearsTheField(t *testing.T) {
	r, mr := newTestReporter(t)
	ctx := context.Background()
	if err := r.SetAborted(ctx, "stalled", 0); err != nil {
		t.Fatal(err)
	}
	if got := mr.HGet("ota", "download-skip-checks:mdb"); got != "" {
		t.Errorf("download-skip-checks:mdb = %q, want empty when no backoff applies", got)
	}
}

func TestInitialize_LeavesAbortFieldsIntact(t *testing.T) {
	r, mr := newTestReporter(t)
	ctx := context.Background()

	if err := r.SetAborted(ctx, "budget-exceeded", 4); err != nil {
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
	if got := mr.HGet("ota", "download-skip-checks:mdb"); got != "4" {
		t.Errorf("download-skip-checks:mdb = %q, want it to survive Initialize", got)
	}
}

func TestSetDownloading_ClearsAbortFields(t *testing.T) {
	r, mr := newTestReporter(t)
	ctx := context.Background()
	if err := r.SetAborted(ctx, "stalled", 4); err != nil {
		t.Fatal(err)
	}
	if err := r.SetDownloading(ctx, "v1.2.3", "full"); err != nil {
		t.Fatal(err)
	}
	if got := mr.HGet("ota", "download-abort-reason:mdb"); got != "" {
		t.Errorf("download-abort-reason:mdb = %q, want cleared by a fresh attempt", got)
	}
	if got := mr.HGet("ota", "download-skip-checks:mdb"); got != "" {
		t.Errorf("download-skip-checks:mdb = %q, want cleared by a fresh attempt", got)
	}
}

func TestSetSkipChecksRemaining_UpdatesTheField(t *testing.T) {
	r, mr := newTestReporter(t)
	ctx := context.Background()
	if err := r.SetAborted(ctx, "stalled", 4); err != nil {
		t.Fatal(err)
	}

	if err := r.SetSkipChecksRemaining(ctx, 3); err != nil {
		t.Fatal(err)
	}
	// SetSkipChecksRemaining is async like SetHeartbeat and SetDownloadProgress,
	// so poll rather than assert immediately after the call returns.
	waitForField(t, mr, "download-skip-checks:mdb", "3")
}

func TestSetSkipChecksRemaining_ZeroClearsTheField(t *testing.T) {
	r, mr := newTestReporter(t)
	ctx := context.Background()
	if err := r.SetAborted(ctx, "stalled", 4); err != nil {
		t.Fatal(err)
	}

	if err := r.SetSkipChecksRemaining(ctx, 0); err != nil {
		t.Fatal(err)
	}
	waitForField(t, mr, "download-skip-checks:mdb", "")
}

// waitForField polls an ota hash field until it matches want or a deadline
// passes, for asserting on a Reporter write that is async by design.
func waitForField(t *testing.T, mr *miniredis.Miniredis, field, want string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	var got string
	for time.Now().Before(deadline) {
		got = mr.HGet("ota", field)
		if got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Errorf("%s = %q, want %q", field, got, want)
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

func TestClearHeartbeat_ClearsField(t *testing.T) {
	r, mr := newTestReporter(t)
	ctx := context.Background()

	if err := r.SetHeartbeat(ctx, time.Unix(1786298400, 0)); err != nil {
		t.Fatal(err)
	}
	waitForField(t, mr, "heartbeat:mdb", "1786298400")

	if err := r.ClearHeartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	waitForField(t, mr, "heartbeat:mdb", "")
}

func TestSetPreviewChecking_ClearsPreviousResult(t *testing.T) {
	r, mr := newTestReporter(t)
	ctx := context.Background()

	if err := r.SetPreviewResult(ctx, "stable", PreviewReady, "v1.3.0", 401234432); err != nil {
		t.Fatal(err)
	}
	if err := r.SetPreviewChecking(ctx, "nightly"); err != nil {
		t.Fatal(err)
	}

	if got := mr.HGet("ota", "preview-channel:mdb"); got != "nightly" {
		t.Errorf("preview-channel:mdb = %q, want nightly", got)
	}
	if got := mr.HGet("ota", "preview-status:mdb"); got != PreviewChecking {
		t.Errorf("preview-status:mdb = %q, want %q", got, PreviewChecking)
	}
	// The stable answer must not stay visible under the nightly channel label.
	if got := mr.HGet("ota", "preview-version:mdb"); got != "" {
		t.Errorf("preview-version:mdb = %q, want cleared", got)
	}
	if got := mr.HGet("ota", "preview-size:mdb"); got != "" {
		t.Errorf("preview-size:mdb = %q, want cleared", got)
	}
}

func TestSetPreviewResult_OmitsSizeWhenUnknown(t *testing.T) {
	r, mr := newTestReporter(t)

	if err := r.SetPreviewResult(context.Background(), "testing", PreviewUnavailable, "", 0); err != nil {
		t.Fatal(err)
	}

	if got := mr.HGet("ota", "preview-status:mdb"); got != PreviewUnavailable {
		t.Errorf("preview-status:mdb = %q, want %q", got, PreviewUnavailable)
	}
	// Not "0": a zero-byte download and an unknown size are different claims.
	if got := mr.HGet("ota", "preview-size:mdb"); got != "" {
		t.Errorf("preview-size:mdb = %q, want empty", got)
	}
}

// A preview left over from before a restart is not an answer to any question
// the UI is currently asking.
func TestInitialize_ClearsPreviewFields(t *testing.T) {
	r, mr := newTestReporter(t)
	ctx := context.Background()

	if err := r.SetPreviewResult(ctx, "stable", PreviewReady, "v1.3.0", 401234432); err != nil {
		t.Fatal(err)
	}
	if err := r.Initialize(ctx, "delta"); err != nil {
		t.Fatal(err)
	}

	for _, field := range []string{"preview-channel:mdb", "preview-status:mdb", "preview-version:mdb", "preview-size:mdb"} {
		if got := mr.HGet("ota", field); got != "" {
			t.Errorf("%s = %q, want cleared after Initialize", field, got)
		}
	}
}
