package updater

import (
	"context"
	"errors"
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"github.com/librescoot/update-service/internal/commitgate"
	"github.com/librescoot/update-service/internal/config"
	"github.com/librescoot/update-service/internal/mender"
	"github.com/librescoot/update-service/internal/redis"
	"github.com/librescoot/update-service/internal/status"
)

func newPendingCommitUpdater(t *testing.T) (*Updater, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rc, err := redis.New(mr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rc.Close() })
	logger := log.New(io.Discard, "", 0)
	dir := t.TempDir()
	return &Updater{
		activationAttempt: filepath.Join(dir, "activation"),
		gateStore:         commitgate.NewStoreAt(filepath.Join(dir, "gate"), filepath.Join(dir, "quarantine")),
		bootID:            func() (string, error) { return "boot-current", nil },
		bootState: func() (mender.BootState, error) {
			return mender.BootState{RunningSlot: "slot-b", OtherSlot: "slot-a", SelectedSlot: "slot-b", UpgradeAvailable: true}, nil
		},
		rootfsOrigin: func() (mender.RootfsOrigin, error) {
			return mender.RootfsOrigin{OriginalSlot: "slot-a", Artifact: "release-v1.4.0", CommittedArtifact: "release-v1.3.1"}, nil
		},
		config: &config.Config{Component: "mdb"},
		redis:  rc,
		status: status.NewReporter(rc.GetClient(), "mdb", logger),
		logger: logger,
		ctx:    context.Background(),
	}, mr
}

func TestPendingCommitVerifiesBeforeAndAfterCommit(t *testing.T) {
	u, mr := newPendingCommitUpdater(t)
	var calls []string
	observations := []mender.UpdateObservation{
		{State: mender.StateNeedsCommit, CommittedArtifact: "release-v1.3.1", PendingArtifact: "release-v1.4.0", PendingVersion: "v1.4.0"},
		{State: mender.StateNoUpdate, CommittedArtifact: "release-v1.4.0", CommittedVersion: "v1.4.0"},
	}
	u.observeUpdate = func() (mender.UpdateObservation, error) {
		calls = append(calls, "observe")
		observation := observations[0]
		observations = observations[1:]
		return observation, nil
	}
	u.runningVersion = func() (string, error) {
		calls = append(calls, "running")
		return "v1.4.0", nil
	}
	u.commitUpdate = func() error {
		calls = append(calls, "commit")
		u.bootState = func() (mender.BootState, error) {
			return mender.BootState{RunningSlot: "slot-b", OtherSlot: "slot-a", SelectedSlot: "slot-b"}, nil
		}
		return nil
	}

	reconciliation, err := u.ReconcilePendingUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if reconciliation.NeedsReboot {
		t.Fatal("commit-complete update still needs reboot")
	}
	if got := strings.Join(calls, ","); !strings.HasPrefix(got, "observe,running,") || !strings.HasSuffix(got, "commit,observe") {
		t.Fatalf("commit was not surrounded by verification: %v", calls)
	}
	if got := mr.HGet("ota", "status:mdb"); got != "idle" {
		t.Fatalf("status = %q, want idle", got)
	}
	if got := mr.HGet("ota", "update-version:mdb"); got != "" {
		t.Fatalf("target version was not cleared after verified commit: %q", got)
	}
}

func TestPendingCommitRecognizesPreRebootCommittedRootfs(t *testing.T) {
	u, _ := newPendingCommitUpdater(t)
	u.bootState = func() (mender.BootState, error) {
		return mender.BootState{RunningSlot: "slot-a", OtherSlot: "slot-b", SelectedSlot: "slot-b", UpgradeAvailable: true}, nil
	}
	u.observeUpdate = func() (mender.UpdateObservation, error) {
		return mender.UpdateObservation{
			State: mender.StateNeedsCommit, CommittedArtifact: "release-v1.3.1",
			CommittedVersion: "v1.3.1", PendingArtifact: "release-v1.4.0", PendingVersion: "v1.4.0",
		}, nil
	}
	u.runningVersion = func() (string, error) { return "v1.3.1", nil }
	committed := false
	u.commitUpdate = func() error { committed = true; return nil }

	reconciliation, err := u.ReconcilePendingUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if !reconciliation.NeedsReboot {
		t.Fatal("pre-reboot Mender commit-enter state did not request reboot")
	}
	if committed {
		t.Fatal("pre-reboot rootfs was committed")
	}
}

func TestPendingRedisTargetMustMatchCommittedArtifact(t *testing.T) {
	u, mr := newPendingCommitUpdater(t)
	mr.HSet("ota", "status:mdb", "pending-reboot")
	mr.HSet("ota", "update-version:mdb", "v1.4.0")
	u.observeUpdate = func() (mender.UpdateObservation, error) {
		return mender.UpdateObservation{State: mender.StateNoUpdate, CommittedArtifact: "release-v1.3.1", CommittedVersion: "v1.3.1"}, nil
	}

	if _, err := u.ReconcilePendingUpdate(); err == nil {
		t.Fatal("expected committed artifact mismatch to fail")
	}
	if got := mr.HGet("ota", "status:mdb"); got != "error" {
		t.Fatalf("status = %q, want error", got)
	}
}

func TestPendingCommitRejectsWrongRunningVersion(t *testing.T) {
	u, mr := newPendingCommitUpdater(t)
	u.observeUpdate = func() (mender.UpdateObservation, error) {
		return mender.UpdateObservation{State: mender.StateNeedsCommit, PendingArtifact: "release-v1.4.0", PendingVersion: "v1.4.0"}, nil
	}
	u.runningVersion = func() (string, error) { return "v1.3.1", nil }
	u.commitUpdate = func() error { return errors.New("must not be called") }

	if _, err := u.ReconcilePendingUpdate(); err == nil {
		t.Fatal("expected mismatched running version to fail")
	}
	if got := mr.HGet("ota", "status:mdb"); got != "error" {
		t.Fatalf("status = %q, want error", got)
	}
}
