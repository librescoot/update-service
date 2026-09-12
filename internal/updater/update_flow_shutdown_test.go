package updater

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/librescoot/update-service/internal/config"
	"github.com/librescoot/update-service/internal/inhibitor"
	"github.com/librescoot/update-service/internal/mender"
	"github.com/librescoot/update-service/internal/redis"
	"github.com/librescoot/update-service/internal/status"
)

// Path-level coverage for the shutdown guard: a canceled updater context must
// leave no terminal error status behind on any update entry point, while
// genuine failures with a live context must still be reported. The shared
// helpers are covered by TestSkipTerminalErrorOnShutdown and
// TestSetRebootTriggerErrorSuppressesOnlyOnShutdown; these tests pin the guard
// placement at the call sites — a placement mistake (a missed inline SetError)
// would pass helper-only tests.

func newUpdateFlowUpdater(t *testing.T) (*Updater, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rc, err := redis.New(mr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rc.Close() })
	logger := log.New(io.Discard, "", 0)
	u := &Updater{
		config:     &config.Config{Component: "mdb"},
		redis:      rc,
		status:     status.NewReporter(rc.GetClient(), "mdb", logger),
		bootStatus: status.NewReporter(rc.GetClient(), "mdb-boot", logger),
		inhibitor:  inhibitor.New(rc.GetClient(), logger),
		logger:     logger,
		mender:     mender.NewManager(t.TempDir(), func() mender.Budget { return mender.Budget{} }, logger),
		ctx:        context.Background(),
	}
	return u, mr
}

// serveMender serves a fake artifact body over HTTP so handleUpdateFromURL can
// reach the install step without a real Mender artifact (install is stubbed).
func serveMender(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := "fake artifact body"
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		if r.Method == http.MethodHead {
			return
		}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/librescoot-unu-mdb-nightly-20260912T062254.mender"
}

func writeFakeArtifact(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "librescoot-unu-mdb-nightly-20260912T062254.mender")
	if err := os.WriteFile(source, []byte("fake artifact body"), 0o644); err != nil {
		t.Fatal(err)
	}
	return source
}

func TestHandleUpdateFromFileShutdownDuringInstallLeavesNoTerminalStatus(t *testing.T) {
	source := writeFakeArtifact(t)
	u, _ := newUpdateFlowUpdater(t)
	ctx, cancel := context.WithCancel(context.Background())
	u.ctx = ctx
	// The install failure races the shutdown: cancel before the failure returns.
	u.installArtifact = func(string, mender.InstallProgressCallback) error {
		cancel()
		return errors.New("install boom")
	}
	u.handleUpdateFromFile(source)

	got, err := u.status.GetStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "installing" {
		t.Fatalf("status after shutdown install failure = %q, want installing (guard reached, no error write)", got)
	}
}

func TestHandleUpdateFromFileLiveInstallFailureStillReportsError(t *testing.T) {
	source := writeFakeArtifact(t)
	u, _ := newUpdateFlowUpdater(t)
	u.installArtifact = func(string, mender.InstallProgressCallback) error {
		return errors.New("install boom")
	}
	u.handleUpdateFromFile(source)

	got, err := u.status.GetStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "error" {
		t.Fatalf("live install failure status = %q, want error", got)
	}
}

func TestHandleUpdateFromURLShutdownDuringInstallLeavesNoTerminalStatus(t *testing.T) {
	u, _ := newUpdateFlowUpdater(t)
	ctx, cancel := context.WithCancel(context.Background())
	u.ctx = ctx
	u.installArtifact = func(string, mender.InstallProgressCallback) error {
		cancel()
		return errors.New("install boom")
	}
	u.handleUpdateFromURL(serveMender(t))

	got, err := u.status.GetStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "installing" {
		t.Fatalf("status after shutdown URL install failure = %q, want installing (guard reached, no error write)", got)
	}
}

func TestHandleUpdateFromURLShutdownRebootTriggerKeepsPendingReboot(t *testing.T) {
	u, mr := newUpdateFlowUpdater(t)
	// Keep the vehicle outside the allowed states so TriggerReboot waits and
	// returns context.Canceled once the shutdown cancels the context.
	mr.HSet("vehicle", "state", "running")
	ctx, cancel := context.WithCancel(context.Background())
	u.ctx = ctx
	u.installArtifact = func(string, mender.InstallProgressCallback) error {
		cancel()
		return nil
	}
	u.handleUpdateFromURL(serveMender(t))

	got, err := u.status.GetStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "pending-reboot" {
		t.Fatalf("status after shutdown URL reboot trigger failure = %q, want pending-reboot", got)
	}
}

func TestHandleUpdateFromURLDryRunShutdownStillWritesIdle(t *testing.T) {
	u, _ := newUpdateFlowUpdater(t)
	u.config.DryRun = true
	ctx, cancel := context.WithCancel(context.Background())
	u.ctx = ctx
	// Cancel during install so the shutdown races the dry-run reboot decision.
	u.installArtifact = func(string, mender.InstallProgressCallback) error {
		cancel()
		return nil
	}
	u.handleUpdateFromURL(serveMender(t))

	// A dry run is a simulated state: idle must be written even when the
	// context is already canceled, because nothing real was staged.
	got, err := u.status.GetStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "idle" {
		t.Fatalf("dry-run status after shutdown = %q, want idle", got)
	}
}

// fakeBootReporter records what the boot lifecycle reported so tests can pin
// the exact status writes.
type fakeBootReporter struct {
	installing   int
	pendingCalls int
	errors       []string
}

func (f *fakeBootReporter) SetInstalling(context.Context) error { f.installing++; return nil }
func (f *fakeBootReporter) SetPendingReboot(context.Context) error {
	f.pendingCalls++
	return nil
}
func (f *fakeBootReporter) SetError(_ context.Context, errorType, errorMessage string) error {
	f.errors = append(f.errors, errorType+": "+errorMessage)
	return nil
}

type fakeBootGuard struct {
	addErr error
	adds   int
}

func (g *fakeBootGuard) AddBootInstallInhibit(string, string) error { g.adds++; return g.addErr }
func (g *fakeBootGuard) RemoveBootInstallInhibit(string) error      { return nil }

type fakeBootWriter struct {
	upToDate bool
}

func (w *fakeBootWriter) UpToDate(string) (bool, error)       { return w.upToDate, nil }
func (w *fakeBootWriter) Apply(context.Context, string) error { return nil }

// A shutdown cancellation while waiting for the guarded install to be observed
// must not publish install-failed; the next instance restores the lifecycle.
func TestRunLocalBootUpdateShutdownCancellationDoesNotWriteInstallFailed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reporter := &fakeBootReporter{}
	calls := 0
	_, err := runLocalBootUpdate(ctx, "mdb", bootUpdateDeps{
		writer: &fakeBootWriter{},
		guard:  &fakeBootGuard{},
		status: reporter,
		rootfsState: func() (mender.UpdateState, error) {
			return mender.StateNoUpdate, nil
		},
		powerObserved: func(context.Context, string) (bool, error) {
			calls++
			cancel() // shutdown lands while waiting for the power block ack
			return false, nil
		},
		ackTimeout:   50 * time.Millisecond,
		pollInterval: 5 * time.Millisecond,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runLocalBootUpdate = %v, want context.Canceled", err)
	}
	if len(reporter.errors) != 0 {
		t.Fatalf("shutdown cancellation published boot errors: %v", reporter.errors)
	}
}

func TestRunLocalBootUpdateGenuineFailureStillWritesInstallFailed(t *testing.T) {
	reporter := &fakeBootReporter{}
	_, err := runLocalBootUpdate(context.Background(), "mdb", bootUpdateDeps{
		writer: &fakeBootWriter{},
		guard:  &fakeBootGuard{addErr: errors.New("acquire boom")},
		status: reporter,
		rootfsState: func() (mender.UpdateState, error) {
			return mender.StateNoUpdate, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "acquire boot power block") {
		t.Fatalf("runLocalBootUpdate = %v, want boot power block failure", err)
	}
	if len(reporter.errors) != 1 || !strings.HasPrefix(reporter.errors[0], "install-failed: ") {
		t.Fatalf("genuine failure boot errors = %v, want exactly one install-failed", reporter.errors)
	}
}

// The abort write is suppressed on shutdown and preserved for genuine failures.
func TestRecordBootAbortSuppressesOnlyOnShutdown(t *testing.T) {
	u, _ := newUpdateFlowUpdater(t)

	// Shutdown first: no write may happen at all.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	u.ctx = ctx
	u.recordBootAbort(errors.New("shutdown boom"))
	got, err := u.bootStatus.GetStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got == "error" {
		t.Fatalf("shutdown boot abort recorded a terminal error")
	}

	// A genuine failure with a live context must still publish.
	u.ctx = context.Background()
	u.recordBootAbort(errors.New("validation boom"))
	got, err = u.bootStatus.GetStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "error" {
		t.Fatalf("live boot abort status = %q, want error", got)
	}
}
