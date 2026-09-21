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

func streamFields(values []string) map[string]string {
	fields := make(map[string]string, len(values)/2)
	for i := 0; i+1 < len(values); i += 2 {
		fields[values[i]] = values[i+1]
	}
	return fields
}

func TestSetError_AppendsEveryMessageFromTheOperation(t *testing.T) {
	r, mr := newTestReporter(t)
	ctx := context.Background()

	if err := r.SetDownloading(ctx, "v1.2.3", "delta"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetError(ctx, "delta-failed", "delta checksum mismatch"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetDownloading(ctx, "v1.2.3", "full"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetError(ctx, "download-failed", "full image download timed out"); err != nil {
		t.Fatal(err)
	}

	if got := mr.HGet("ota", "error:mdb"); got != "download-failed" {
		t.Errorf("error:mdb = %q, want latest error type", got)
	}
	if got := mr.HGet("ota", "error-message:mdb"); got != "full image download timed out" {
		t.Errorf("error-message:mdb = %q, want latest message", got)
	}
	entries, err := mr.Stream("ota:errors")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("stream entries = %d, want 2", len(entries))
	}
	first := streamFields(entries[0].Values)
	second := streamFields(entries[1].Values)
	if first["event"] != "error" || first["component"] != "mdb" || first["code"] != "delta-failed" || first["message"] != "delta checksum mismatch" {
		t.Errorf("first stream entry = %v", first)
	}
	if second["event"] != "error" || second["component"] != "mdb" || second["code"] != "download-failed" || second["message"] != "full image download timed out" {
		t.Errorf("second stream entry = %v", second)
	}
}

func TestSetError_AppendsRepeatedMessages(t *testing.T) {
	r, mr := newTestReporter(t)
	ctx := context.Background()

	if err := r.SetError(ctx, "reboot-failed", "reboot unavailable"); err != nil {
		t.Fatal(err)
	}
	firstEvent := mr.HGet("ota", "error-event:mdb")
	if err := r.SetError(ctx, "reboot-failed", "reboot unavailable"); err != nil {
		t.Fatal(err)
	}
	if secondEvent := mr.HGet("ota", "error-event:mdb"); secondEvent == firstEvent {
		t.Errorf("error-event:mdb did not change: %q", secondEvent)
	}

	entries, err := mr.Stream("ota:errors")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("stream entries = %d, want both occurrences", len(entries))
	}
}

func TestSetIdle_AppendsErrorReset(t *testing.T) {
	r, mr := newTestReporter(t)
	ctx := context.Background()

	if err := r.SetError(ctx, "download-failed", "network unavailable"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetIdle(ctx); err != nil {
		t.Fatal(err)
	}

	entries, err := mr.Stream("ota:errors")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("stream entries = %d, want error and reset", len(entries))
	}
	reset := streamFields(entries[1].Values)
	if reset["event"] != "reset" || reset["component"] != "mdb" {
		t.Errorf("reset stream entry = %v", reset)
	}
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

func TestSetDBCPreflight_DoesNotChangeUpdateLifecycle(t *testing.T) {
	r, mr := newTestReporter(t)
	ctx := context.Background()
	if err := r.SetDownloading(ctx, "v1.2.3", "full"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetDBCPreflight(ctx, DBCPreflightAvailable, "v1.3.0"); err != nil {
		t.Fatal(err)
	}

	if got := mr.HGet("ota", "preflight-result:mdb"); got != DBCPreflightAvailable {
		t.Errorf("preflight-result:mdb = %q, want %q", got, DBCPreflightAvailable)
	}
	if got := mr.HGet("ota", "preflight-version:mdb"); got != "v1.3.0" {
		t.Errorf("preflight-version:mdb = %q, want v1.3.0", got)
	}
	if got := mr.HGet("ota", "preflight-time:mdb"); got == "" {
		t.Error("preflight-time:mdb was not published")
	}
	if got := mr.HGet("ota", "status:mdb"); got != string(StatusDownloading) {
		t.Errorf("status:mdb = %q, want downloading", got)
	}
	if got := mr.HGet("ota", "update-version:mdb"); got != "v1.2.3" {
		t.Errorf("update-version:mdb = %q, want v1.2.3", got)
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

func TestSetCommitGate(t *testing.T) {
	r, mr := newTestReporter(t)
	ctx := context.Background()

	deadline := time.Date(2026, 9, 21, 19, 3, 31, 0, time.UTC)
	if err := r.SetCommitGate(ctx, "waiting", "units: librescoot-pm.service", deadline); err != nil {
		t.Fatal(err)
	}
	if got := mr.HGet("ota", "commit-gate:mdb"); got != "waiting" {
		t.Errorf("commit-gate:mdb = %q, want waiting", got)
	}
	if got := mr.HGet("ota", "commit-gate-reason:mdb"); got != "units: librescoot-pm.service" {
		t.Errorf("commit-gate-reason:mdb = %q", got)
	}
	if got := mr.HGet("ota", "commit-gate-deadline:mdb"); got != "2026-09-21T19:03:31Z" {
		t.Errorf("commit-gate-deadline:mdb = %q, want the RFC3339 deadline", got)
	}

	// A verdict has nothing left to wait for.
	if err := r.SetCommitGate(ctx, "rolled-back", "deadline", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got := mr.HGet("ota", "commit-gate:mdb"); got != "rolled-back" {
		t.Errorf("commit-gate:mdb = %q, want rolled-back", got)
	}
	if got := mr.HGet("ota", "commit-gate-deadline:mdb"); got != "" {
		t.Errorf("commit-gate-deadline:mdb = %q, want cleared", got)
	}
}

// A terminal transition means the component is not being gated any more.
func TestTerminalStatusClearsCommitGateFields(t *testing.T) {
	r, mr := newTestReporter(t)
	ctx := context.Background()

	if err := r.SetCommitGate(ctx, "waiting", "uptime", time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := r.SetIdle(ctx); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"commit-gate:mdb", "commit-gate-reason:mdb", "commit-gate-deadline:mdb"} {
		if got := mr.HGet("ota", field); got != "" {
			t.Errorf("%s = %q, want cleared by a terminal status", field, got)
		}
	}
}

func TestInitializeClearsCommitGateFields(t *testing.T) {
	r, mr := newTestReporter(t)
	ctx := context.Background()

	if err := r.SetCommitGate(ctx, "waiting", "uptime", time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := r.Initialize(ctx, "delta"); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"commit-gate:mdb", "commit-gate-reason:mdb", "commit-gate-deadline:mdb"} {
		if got := mr.HGet("ota", field); got != "" {
			t.Errorf("%s = %q, want cleared after Initialize", field, got)
		}
	}
}
