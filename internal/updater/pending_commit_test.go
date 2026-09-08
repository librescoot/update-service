package updater

import (
	"context"
	"errors"
	"io"
	"log"
	"reflect"
	"testing"

	"github.com/alicebob/miniredis/v2"

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
	return &Updater{
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
		return nil
	}

	needsReboot, err := u.CheckAndCommitPendingUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if needsReboot {
		t.Fatal("commit-complete update still needs reboot")
	}
	if want := []string{"observe", "running", "commit", "observe"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
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
	u.observeUpdate = func() (mender.UpdateObservation, error) {
		return mender.UpdateObservation{
			State: mender.StateNeedsCommit, CommittedArtifact: "release-v1.3.1",
			CommittedVersion: "v1.3.1", PendingArtifact: "release-v1.4.0", PendingVersion: "v1.4.0",
		}, nil
	}
	u.runningVersion = func() (string, error) { return "v1.3.1", nil }
	committed := false
	u.commitUpdate = func() error { committed = true; return nil }

	needsReboot, err := u.CheckAndCommitPendingUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if !needsReboot {
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

	if _, err := u.CheckAndCommitPendingUpdate(); err == nil {
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

	if _, err := u.CheckAndCommitPendingUpdate(); err == nil {
		t.Fatal("expected mismatched running version to fail")
	}
	if got := mr.HGet("ota", "status:mdb"); got != "error" {
		t.Fatalf("status = %q, want error", got)
	}
}
