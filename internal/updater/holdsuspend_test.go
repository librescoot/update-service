package updater

import (
	"log"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/librescoot/update-service/internal/config"
	"github.com/librescoot/update-service/internal/inhibitor"
	ipc "github.com/librescoot/redis-ipc"
)

// newTestUpdaterForInhibits builds an Updater with just enough wired up to
// exercise holdSuspend: a real inhibitor.Client backed by miniredis, matching
// the pattern already used for status.Reporter tests.
func newTestUpdaterForInhibits(t *testing.T, maxDuration time.Duration) (*Updater, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client, err := ipc.New(ipc.WithURL(mr.Addr()), ipc.WithCodec(ipc.StringCodec{}))
	if err != nil {
		t.Fatalf("connecting test client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	logger := log.New(os.Stdout, "test: ", 0)
	u := &Updater{
		config: &config.Config{
			Component:           "mdb",
			DownloadMaxDuration: maxDuration,
		},
		inhibitor: inhibitor.New(client, logger),
		logger:    logger,
	}
	return u, mr
}

func TestHoldSuspend_TakesAndReleasesInhibit(t *testing.T) {
	u, mr := newTestUpdaterForInhibits(t, time.Hour)

	release := u.holdSuspend()

	if !mr.Exists("power:inhibits") || mr.HGet("power:inhibits", "download-transfer:mdb") == "" {
		t.Fatal("expected download-transfer:mdb inhibit to be present after holdSuspend")
	}

	release()

	if got := mr.HGet("power:inhibits", "download-transfer:mdb"); got != "" {
		t.Errorf("expected download-transfer:mdb inhibit to be removed after release, got %q", got)
	}
}

func TestHoldSuspend_DisabledBudgetTakesNoInhibit(t *testing.T) {
	u, mr := newTestUpdaterForInhibits(t, 0)

	release := u.holdSuspend()

	if got := mr.HGet("power:inhibits", "download-transfer:mdb"); got != "" {
		t.Errorf("expected no inhibit to be taken when download-max-duration is disabled, got %q", got)
	}

	// The release func must still be safe to call unconditionally, matching
	// every call site's defer u.holdSuspend()().
	release()

	if got := mr.HGet("power:inhibits", "download-transfer:mdb"); got != "" {
		t.Errorf("release of a no-op hold must not create an inhibit, got %q", got)
	}
}

func TestHoldSuspend_NegativeBudgetTakesNoInhibit(t *testing.T) {
	u, mr := newTestUpdaterForInhibits(t, -1*time.Second)

	release := u.holdSuspend()
	defer release()

	if got := mr.HGet("power:inhibits", "download-transfer:mdb"); got != "" {
		t.Errorf("expected no inhibit for a negative download-max-duration, got %q", got)
	}
}
