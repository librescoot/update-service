package updater

import (
	"context"
	"errors"
	"log"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	ipc "github.com/librescoot/redis-ipc"

	"github.com/librescoot/update-service/internal/backoff"
	"github.com/librescoot/update-service/internal/config"
	"github.com/librescoot/update-service/internal/mender"
	"github.com/librescoot/update-service/internal/status"
)

// newTestUpdaterForAbort builds an Updater with a real backoff.Store (a temp
// directory, same as backoff's own tests) and a real status.Reporter backed
// by miniredis, enough to exercise recordDownloadAbort without needing
// network, mender, or Redis-command scaffolding this package doesn't have.
func newTestUpdaterForAbort(t *testing.T) (*Updater, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client, err := ipc.New(ipc.WithURL(mr.Addr()), ipc.WithCodec(ipc.StringCodec{}))
	if err != nil {
		t.Fatalf("connecting test client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	logger := log.New(os.Stdout, "test: ", 0)
	u := &Updater{
		config:  &config.Config{Component: "mdb", CheckInterval: time.Hour},
		backoff: backoff.NewStore(t.TempDir(), logger),
		status:  status.NewReporter(client, "mdb", logger),
		logger:  logger,
		ctx:     context.Background(),
	}
	return u, mr
}

// This is the "DOES record an abort" half of the finding-5 requirement
// ("a delta budget abort does NOT call fallbackToFullUpdate and DOES record
// an abort"): recordDownloadAbort is what performDeltaUpdate's budget-abort
// branch calls before returning (internal/updater/updater.go, the
// isDownloadBudgetAbort branch in the per-delta retry loop). The "does NOT
// call fallbackToFullUpdate" half is a pure control-flow fact in that same
// branch (it returns immediately after this call, never reaching
// abandonChain/fallbackToFullUpdate the way the ErrAssetUnavailable and
// checksum-limit branches below it do) - verified by reading the code rather
// than by an automated test, since exercising that branch end-to-end needs a
// full Updater with a working GitHub API and mender.Manager pointed at a
// fake server, scaffolding this package does not have and which is out of
// scope for this fix.
func TestRecordDownloadAbort_BudgetExceeded(t *testing.T) {
	u, mr := newTestUpdaterForAbort(t)

	u.recordDownloadAbort("v1.2.3", mender.ErrDownloadBudgetExceeded, 100)

	waitForField(t, mr, "ota", "download-abort-reason:mdb", "budget-exceeded")
	if got := mr.HGet("ota", "download-skip-checks:mdb"); got == "" {
		t.Error("expected download-skip-checks:mdb to be set after the first abort")
	}
	if got := mr.HGet("ota", "status:mdb"); got != string(status.StatusIdle) {
		t.Errorf("status:mdb = %q, want idle (SetAborted returns to idle)", got)
	}
}

func TestRecordDownloadAbort_Stalled(t *testing.T) {
	u, mr := newTestUpdaterForAbort(t)

	u.recordDownloadAbort("v1.2.3", mender.ErrDownloadStalled, 100)

	waitForField(t, mr, "ota", "download-abort-reason:mdb", "stalled")
}

// TestRecordDownloadAbort_ProgressResetsLadder pins the productive-attempt
// exception: an attempt that moved ProgressResetBytes or more resets the
// ladder (rung -1) rather than advancing it, so no skip-checks are recorded.
func TestRecordDownloadAbort_ProgressResetsLadder(t *testing.T) {
	u, mr := newTestUpdaterForAbort(t)

	u.recordDownloadAbort("v1.2.3", mender.ErrDownloadBudgetExceeded, backoff.ProgressResetBytes)

	waitForField(t, mr, "ota", "download-abort-reason:mdb", "budget-exceeded")
	if got := mr.HGet("ota", "download-skip-checks:mdb"); got != "" {
		t.Errorf("download-skip-checks:mdb = %q, want empty: a productive attempt must not be backed off", got)
	}
}

// waitForField polls an ota hash field until it matches want, for asserting
// on a Reporter write that is async by design (SetAborted uses ipc.Sync(),
// but recordDownloadAbort itself has no synchronous signal back to the
// caller either way, so poll defensively rather than assume it landed).
func waitForField(t *testing.T, mr *miniredis.Miniredis, hash, field, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		got = mr.HGet(hash, field)
		if got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s.%s = %q, want %q", hash, field, got, want)
}

// TestIsDownloadBudgetAbort_Classification pins the classifier that
// performDeltaUpdate's retry loop and performUpdateLocked both dispatch on:
// isDownloadBudgetAbort must be true for the two budget sentinels and false
// for every other error, in particular ErrAssetUnavailable, which takes the
// opposite branch (abandonChain/fallbackToFullUpdate, not a bare return -
// see finding 5's "ErrAssetUnavailable still DOES fall back"). A regression
// here would silently swap which of the two branches an error takes.
func TestIsDownloadBudgetAbort_Classification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"stalled", mender.ErrDownloadStalled, true},
		{"budget exceeded", mender.ErrDownloadBudgetExceeded, true},
		{"asset unavailable falls back, not a budget abort", mender.ErrAssetUnavailable, false},
		{"checksum mismatch falls back, not a budget abort", mender.ErrChecksumMismatch, false},
		{"generic error", errors.New("connection reset"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDownloadBudgetAbort(tc.err); got != tc.want {
				t.Errorf("isDownloadBudgetAbort(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
