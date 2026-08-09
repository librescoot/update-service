package updater

import (
	"context"
	"log"
	"os"
	"sync"
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

// TestStartHeartbeat_NestedCallsShareOneGoroutine covers the exact scenario
// finding 4 is about: performDeltaUpdate's fallback path calls
// performUpdateLocked directly while its own heartbeat is still running.
// Both hold a live startHeartbeat() reference at once; this must not spawn a
// second goroutine, and the field must survive until the last one stops.
func TestStartHeartbeat_NestedCallsShareOneGoroutine(t *testing.T) {
	u, mr := newTestUpdaterForHeartbeat(t)

	outerStop := u.startHeartbeat()
	waitForNonEmptyHeartbeatField(t, mr)

	u.heartbeatMu.Lock()
	countAfterOuter := u.heartbeatCount
	u.heartbeatMu.Unlock()
	if countAfterOuter != 1 {
		t.Fatalf("heartbeatCount after first start = %d, want 1", countAfterOuter)
	}

	innerStop := u.startHeartbeat()

	u.heartbeatMu.Lock()
	countAfterInner := u.heartbeatCount
	u.heartbeatMu.Unlock()
	if countAfterInner != 2 {
		t.Fatalf("heartbeatCount after nested start = %d, want 2", countAfterInner)
	}

	// The inner (nested) operation finishing first must not clear the field
	// or stop the goroutine: the outer operation is still in progress.
	innerStop()
	if got := mr.HGet("ota", "heartbeat:mdb"); got == "" {
		t.Error("nested stop cleared the field while the outer operation is still running")
	}
	u.heartbeatMu.Lock()
	countAfterInnerStop := u.heartbeatCount
	u.heartbeatMu.Unlock()
	if countAfterInnerStop != 1 {
		t.Fatalf("heartbeatCount after inner stop = %d, want 1", countAfterInnerStop)
	}

	outerStop()
	waitForHeartbeatField(t, mr, "")
	u.heartbeatMu.Lock()
	countAfterOuterStop := u.heartbeatCount
	u.heartbeatMu.Unlock()
	if countAfterOuterStop != 0 {
		t.Fatalf("heartbeatCount after outer stop = %d, want 0", countAfterOuterStop)
	}

	done := make(chan struct{})
	go func() {
		u.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat goroutine leaked past the last stop()")
	}
}

// TestStartHeartbeat_RefcountNeverGoesNegative calls every stop() twice
// across several nested starts, which must not push heartbeatCount below
// zero or spawn an extra goroutine on the next start.
func TestStartHeartbeat_RefcountNeverGoesNegative(t *testing.T) {
	u, mr := newTestUpdaterForHeartbeat(t)

	var stops []func()
	for range 3 {
		stops = append(stops, u.startHeartbeat())
	}
	waitForNonEmptyHeartbeatField(t, mr)

	for _, stop := range stops {
		stop()
		stop() // double-call every one of them
	}

	u.heartbeatMu.Lock()
	count := u.heartbeatCount
	u.heartbeatMu.Unlock()
	if count != 0 {
		t.Fatalf("heartbeatCount = %d after all stops (each called twice), want 0", count)
	}
	waitForHeartbeatField(t, mr, "")

	done := make(chan struct{})
	go func() {
		u.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat goroutine leaked after over-stopping")
	}

	// A fresh start after the count bottomed out at zero must still spawn a
	// working goroutine rather than being wedged by a negative excursion.
	stop := u.startHeartbeat()
	waitForNonEmptyHeartbeatField(t, mr)
	stop()
	waitForHeartbeatField(t, mr, "")
}

// TestStartHeartbeat_ConcurrentStartStop hammers startHeartbeat from many
// goroutines at once (run with -race). It doesn't assert on ordering, only
// that the refcount always lands back at zero and the goroutine always
// exits, i.e. no lost decrement and no leak under real concurrency.
func TestStartHeartbeat_ConcurrentStartStop(t *testing.T) {
	u, _ := newTestUpdaterForHeartbeat(t)

	const workers = 20
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			stop := u.startHeartbeat()
			time.Sleep(time.Millisecond)
			stop()
		}()
	}
	wg.Wait()

	u.heartbeatMu.Lock()
	count := u.heartbeatCount
	u.heartbeatMu.Unlock()
	if count != 0 {
		t.Fatalf("heartbeatCount = %d after all concurrent workers finished, want 0", count)
	}

	done := make(chan struct{})
	go func() {
		u.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat goroutine leaked after concurrent start/stop")
	}
}
