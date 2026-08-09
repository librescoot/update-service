package updater

import (
	"context"
	"log"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	ipc "github.com/librescoot/redis-ipc"

	"github.com/librescoot/update-service/internal/status"
)

// newTestUpdaterForHeartbeat builds an Updater with just enough wired up to
// exercise startHeartbeat: a real status.Reporter backed by miniredis (the
// same pattern status/reporter_test.go already uses), a real context, and a
// zero-value sync.WaitGroup/Mutex, which are ready to use without further
// setup.
func newTestUpdaterForHeartbeat(t *testing.T) (*Updater, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client, err := ipc.New(ipc.WithURL(mr.Addr()), ipc.WithCodec(ipc.StringCodec{}))
	if err != nil {
		t.Fatalf("connecting test client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	logger := log.New(os.Stdout, "test: ", 0)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	u := &Updater{
		status: status.NewReporter(client, "mdb", logger),
		logger: logger,
		ctx:    ctx,
		cancel: cancel,
	}
	return u, mr
}

func waitForHeartbeatField(t *testing.T, mr *miniredis.Miniredis, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		got = mr.HGet("ota", "heartbeat:mdb")
		if got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("heartbeat:mdb = %q, want %q", got, want)
}

// waitForNonEmptyHeartbeatField polls until the field holds any non-empty
// value, without caring what exactly it is.
func waitForNonEmptyHeartbeatField(t *testing.T, mr *miniredis.Miniredis) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if mr.HGet("ota", "heartbeat:mdb") != "" {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("heartbeat:mdb never became non-empty")
}

func TestStartHeartbeat_ClearsFieldOnStop(t *testing.T) {
	u, mr := newTestUpdaterForHeartbeat(t)

	stop := u.startHeartbeat()
	waitForNonEmptyHeartbeatField(t, mr)

	stop()
	waitForHeartbeatField(t, mr, "")
}

func TestStartHeartbeat_ClearsFieldOnContextCancel(t *testing.T) {
	u, mr := newTestUpdaterForHeartbeat(t)

	u.startHeartbeat() // deliberately never stopped

	// Wait for the initial write to land before cancelling: SetHeartbeat and
	// ClearHeartbeat are both async fire-and-forget writes with no ordering
	// guarantee between separate calls, so cancelling immediately could let
	// the clear land before the initial write and produce a false pass.
	waitForNonEmptyHeartbeatField(t, mr)
	u.cancel()

	waitForHeartbeatField(t, mr, "")

	done := make(chan struct{})
	go func() {
		u.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat goroutine did not exit after context cancellation")
	}
}

func TestStartHeartbeat_StopIsIdempotent(t *testing.T) {
	u, mr := newTestUpdaterForHeartbeat(t)

	stop := u.startHeartbeat()
	waitForNonEmptyHeartbeatField(t, mr)
	stop()
	waitForHeartbeatField(t, mr, "")

	// A second call must not panic (double close) and must not resurrect the
	// field or the goroutine.
	stop()
	if got := mr.HGet("ota", "heartbeat:mdb"); got != "" {
		t.Errorf("second stop() call resurrected heartbeat:mdb = %q", got)
	}

	done := make(chan struct{})
	go func() {
		u.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat goroutine leaked past stop()")
	}
}
