package updater

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/librescoot/update-service/internal/mender"
)

var errBootTest = errors.New("injected failure")

type fakeBootOperation struct {
	events                                            []string
	held                                              bool
	writes                                            int
	requestID                                         string
	powerAck                                          bool
	acquireErr, installingErr, applyErr, heartbeatErr error
	ackErr, commandErr, rootfsErr                     error
	state                                             mender.UpdateState
	ack                                               bool
	current                                           bool
	apply                                             func(context.Context) error
}

func (f *fakeBootOperation) UpToDate(string) (bool, error) { return f.current, nil }
func (f *fakeBootOperation) Apply(ctx context.Context, _ string) error {
	f.events = append(f.events, "write")
	if !f.held || !f.powerAck {
		panic("write without processed inhibitor")
	}
	f.writes++
	if f.apply != nil {
		return f.apply(ctx)
	}
	return f.applyErr
}
func (f *fakeBootOperation) AddBootInstallInhibit(_ string, requestID string) error {
	f.requestID = requestID
	f.events = append(f.events, "acquire")
	f.held = f.acquireErr == nil
	return f.acquireErr
}
func (f *fakeBootOperation) RemoveBootInstallInhibit(string) error {
	f.events = append(f.events, "release")
	f.held = false
	return nil
}
func (f *fakeBootOperation) SetInstalling(context.Context) error {
	f.events = append(f.events, "installing")
	return f.installingErr
}
func (f *fakeBootOperation) SetPendingReboot(context.Context) error {
	f.events = append(f.events, "pending")
	return nil
}
func (f *fakeBootOperation) SetError(context.Context, string, string) error {
	f.events = append(f.events, "error")
	return nil
}
func (f *fakeBootOperation) SetHeartbeat(context.Context, time.Time) error {
	f.events = append(f.events, "heartbeat")
	return f.heartbeatErr
}
func (f *fakeBootOperation) ClearHeartbeat(ctx context.Context) error {
	f.events = append(f.events, "clear-heartbeat")
	return ctx.Err()
}
func (f *fakeBootOperation) deps() bootUpdateDeps {
	return bootUpdateDeps{
		writer: f, guard: f, status: f, heartbeat: f,
		rootfsState: func() (mender.UpdateState, error) { return f.state, f.rootfsErr },
		powerObserved: func(_ context.Context, requestID string) (bool, error) {
			if requestID == "" || requestID != f.requestID {
				panic("mismatched request")
			}
			f.powerAck = true
			return true, nil
		},
		command: func(ctx context.Context, s string) error {
			f.events = append(f.events, s)
			if s == "start-dbc" {
				return f.commandErr
			}
			return ctx.Err()
		},
		dbcUpdating: func(context.Context) (bool, error) { f.events = append(f.events, "ack"); return f.ack, f.ackErr },
		ackTimeout:  10 * time.Millisecond, pollInterval: time.Millisecond, heartbeatInterval: time.Hour,
	}
}
func newFakeBoot() *fakeBootOperation {
	return &fakeBootOperation{state: mender.StateNoUpdate, ack: true}
}

func TestBootOperationSerializesWithRootfs(t *testing.T) {
	u := &Updater{}
	f := newFakeBoot()
	u.updateOpMu.Lock()
	applied, err := u.applyLocalBootUpdate(context.Background(), "mdb", f.deps())
	u.updateOpMu.Unlock()
	if applied || err == nil || f.writes != 0 || len(f.events) != 0 {
		t.Fatalf("applied=%v err=%v events=%v", applied, err, f.events)
	}
	f.apply = func(context.Context) error {
		if u.updateOpMu.TryLock() {
			u.updateOpMu.Unlock()
			t.Fatal("boot write not serialized")
		}
		return nil
	}
	if applied, err := u.applyLocalBootUpdate(context.Background(), "mdb", f.deps()); !applied || err != nil {
		t.Fatalf("applied=%v err=%v", applied, err)
	}
	if !u.updateOpMu.TryLock() {
		t.Fatal("operation mutex leaked")
	}
	u.updateOpMu.Unlock()
}

func TestBootOperationGuardOrder(t *testing.T) {
	f := newFakeBoot()
	applied, err := runLocalBootUpdate(context.Background(), "dbc", f.deps())
	if err != nil || !applied {
		t.Fatalf("applied=%v err=%v", applied, err)
	}
	want := []string{"acquire", "installing", "start-dbc", "ack", "heartbeat", "write", "pending", "clear-heartbeat", "complete-dbc", "release"}
	if !reflect.DeepEqual(f.events, want) {
		t.Fatalf("events=%v want=%v", f.events, want)
	}
	if f.held {
		t.Fatal("inhibitor leaked")
	}
}

func TestBootOperationFailsClosed(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(*fakeBootOperation)
		writes int
	}{
		{"inhibit", func(f *fakeBootOperation) { f.acquireErr = errBootTest }, 0},
		{"status", func(f *fakeBootOperation) { f.installingErr = errBootTest }, 0},
		{"start", func(f *fakeBootOperation) { f.commandErr = errBootTest }, 0},
		{"ack redis error", func(f *fakeBootOperation) { f.ackErr = errBootTest }, 0},
		{"ack timeout", func(f *fakeBootOperation) { f.ack = false }, 0},
		{"heartbeat", func(f *fakeBootOperation) { f.heartbeatErr = errBootTest }, 0},
		{"verification", func(f *fakeBootOperation) { f.applyErr = errBootTest }, 1},
		{"rootfs error", func(f *fakeBootOperation) { f.rootfsErr = errBootTest }, 0},
		{"rootfs reboot", func(f *fakeBootOperation) { f.state = mender.StateNeedsReboot }, 0},
		{"rootfs commit", func(f *fakeBootOperation) { f.state = mender.StateNeedsCommit }, 0},
		{"rootfs inconsistent", func(f *fakeBootOperation) { f.state = mender.StateInconsistent }, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeBoot()
			tt.setup(f)
			applied, err := runLocalBootUpdate(context.Background(), "dbc", f.deps())
			if err == nil || applied {
				t.Fatalf("applied=%v err=%v", applied, err)
			}
			if f.writes != tt.writes || f.held {
				t.Fatalf("writes=%d held=%v events=%v", f.writes, f.held, f.events)
			}
			started, completed := false, false
			for _, event := range f.events {
				started = started || event == "start-dbc"
				completed = completed || event == "complete-dbc"
			}
			if started != completed {
				t.Fatalf("unpaired lifecycle: %v", f.events)
			}
		})
	}
}

func TestBootOperationCancellationCleanup(t *testing.T) {
	f := newFakeBoot()
	ctx, cancel := context.WithCancel(context.Background())
	d := f.deps()
	d.dbcUpdating = func(context.Context) (bool, error) { cancel(); return false, nil }
	applied, err := runLocalBootUpdate(ctx, "dbc", d)
	if applied || !errors.Is(err, context.Canceled) || f.writes != 0 || f.held {
		t.Fatalf("applied=%v err=%v events=%v", applied, err, f.events)
	}
	// A shutdown cancellation is not a failure: the deferred status write must
	// be suppressed, so no "error" event is published (the next instance
	// restores the lifecycle).
	if !reflect.DeepEqual(f.events, []string{"acquire", "installing", "start-dbc", "complete-dbc", "release"}) {
		t.Fatal(f.events)
	}
}

func TestBootOperationCancellationDuringWrite(t *testing.T) {
	f := newFakeBoot()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.apply = func(context.Context) error { cancel(); return context.Canceled }
	applied, err := runLocalBootUpdate(ctx, "dbc", f.deps())
	if applied || !errors.Is(err, context.Canceled) || f.held {
		t.Fatalf("applied=%v err=%v held=%v", applied, err, f.held)
	}
	want := []string{"acquire", "installing", "start-dbc", "ack", "heartbeat", "write", "clear-heartbeat", "complete-dbc", "release"}
	if !reflect.DeepEqual(f.events, want) {
		t.Fatalf("events=%v", f.events)
	}
}

func TestBootDBCAcknowledgementPolling(t *testing.T) {
	reads := 0
	err := waitBootDBC(context.Background(), time.Second, time.Millisecond, func(context.Context) (bool, error) {
		reads++
		return reads == 2, nil
	})
	if err != nil || reads != 2 {
		t.Fatalf("reads=%d err=%v", reads, err)
	}
}

func TestBootDBCAcknowledgementReadDeadline(t *testing.T) {
	err := waitBootDBC(context.Background(), time.Millisecond, time.Millisecond, func(ctx context.Context) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func TestBootOperationMDBAndAlreadyCurrent(t *testing.T) {
	f := newFakeBoot()
	if applied, err := runLocalBootUpdate(context.Background(), "mdb", f.deps()); !applied || err != nil {
		t.Fatalf("applied=%v err=%v", applied, err)
	}
	if !reflect.DeepEqual(f.events, []string{"acquire", "installing", "write", "pending", "release"}) {
		t.Fatal(f.events)
	}
	f = newFakeBoot()
	f.current = true
	if applied, err := runLocalBootUpdate(context.Background(), "dbc", f.deps()); applied || err != nil || len(f.events) != 0 {
		t.Fatalf("applied=%v err=%v events=%v", applied, err, f.events)
	}
}

type testBootHeartbeat struct {
	ticks  atomic.Int32
	ticked chan struct{}
	fail   bool
}

func (h *testBootHeartbeat) SetHeartbeat(context.Context, time.Time) error {
	if h.ticks.Add(1) == 2 {
		close(h.ticked)
		if h.fail {
			return errBootTest
		}
	}
	return nil
}
func (h *testBootHeartbeat) ClearHeartbeat(ctx context.Context) error { return ctx.Err() }

func TestBootHeartbeatCoversCancelledWriter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	h := &testBootHeartbeat{ticked: make(chan struct{})}
	stop, err := startBootHeartbeat(ctx, h, time.Millisecond, cancel)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-h.ticked:
	case <-time.After(time.Second):
		t.Fatal("heartbeat stopped before writer returned")
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

func TestBootHeartbeatFailureCancelsWriter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := &testBootHeartbeat{ticked: make(chan struct{}), fail: true}
	stop, err := startBootHeartbeat(ctx, h, time.Millisecond, cancel)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("heartbeat failure did not cancel writer")
	}
	if err := stop(); !errors.Is(err, errBootTest) {
		t.Fatalf("err=%v", err)
	}
}
