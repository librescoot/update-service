package updater

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/librescoot/update-service/internal/commitgate"
	"github.com/librescoot/update-service/internal/config"
	"github.com/librescoot/update-service/internal/dbcstate"
	"github.com/librescoot/update-service/internal/mender"
	"github.com/librescoot/update-service/internal/redis"
	"github.com/librescoot/update-service/internal/status"
)

const (
	gateArtifact = "release-nightly-20260921T003447"
	gateVersion  = "nightly-20260921t003447"
	gatePrevious = "nightly-20260916t091909"
	gateBootA    = "boot-a"
	gateBootB    = "boot-b"
)

// gateHarness is one updater with a controllable clock, scripted probes and a
// gate store in a temporary directory. Every side effect the gate can have is
// recorded, so a test can assert on the decision without a systemd, a vehicle or
// a reboot.
type gateHarness struct {
	updater *Updater
	redis   *miniredis.Miniredis
	store   *commitgate.Store
	now     time.Time

	probes     []gateProbeResult
	commits    int
	rollbacks  int
	reboots    int
	bootID     string
	running    string
	committed  string
	runningErr error
}

func newGateHarness(t *testing.T, opts ...func(*gateHarness)) *gateHarness {
	t.Helper()
	return newGateHarnessFor(t, "mdb", opts...)
}

// newGateHarnessFor is newGateHarness for an explicit component: the gate's
// probe set, the ota hash fields it publishes, and the DBC activation marker all
// follow the component.
func newGateHarnessFor(t *testing.T, component string, opts ...func(*gateHarness)) *gateHarness {
	t.Helper()
	mr := miniredis.RunT(t)
	rc, err := redis.New(mr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rc.Close() })

	logger := log.New(io.Discard, "", 0)
	dir := t.TempDir()
	store := commitgate.NewStoreAt(
		filepath.Join(dir, "commit-gate-mdb.json"),
		filepath.Join(dir, "commit-gate-quarantine-mdb.json"),
	)

	h := &gateHarness{
		redis:   mr,
		store:   store,
		now:     time.Date(2026, 9, 21, 18, 43, 31, 0, time.UTC),
		bootID:  gateBootA,
		running: gateVersion,
		probes:  allGateProbesPassing(),
	}

	cfg := config.New("localhost:6379", "https://example.invalid", time.Hour, component, "nightly", "/data/ota/"+component, false, false, "/uboot", "", 2)
	cfg.SetCommitGate(true)

	h.updater = &Updater{
		config: cfg,
		redis:  rc,
		status: status.NewReporter(rc.GetClient(), component, logger),
		logger: logger,
		ctx:    context.Background(),
		// A cancelled-but-present context keeps TriggerReboot out of the test:
		// gateReboot is substituted below.
		gateStore:         store,
		activationAttempt: filepath.Join(dir, "dbc-activation-attempt"),
		gateNow:           func() time.Time { return h.now },
		gateEvaluate: func(config.CommitGateSettings) []gateProbeResult {
			return h.probes
		},
		gateReboot: func() error { h.reboots++; return nil },
		bootID:     func() (string, error) { return h.bootID, nil },
		runningVersion: func() (string, error) {
			return h.running, h.runningErr
		},
		observeUpdate: func() (mender.UpdateObservation, error) { return h.observation(), nil },
		commitUpdate:  func() error { h.commits++; h.committed = gateArtifact; return nil },
		rollbackUpdate: func() error {
			h.rollbacks++
			h.committed = ""
			return nil
		},
	}
	h.updater.observeUpdate = func() (mender.UpdateObservation, error) { return h.observation(), nil }

	for _, opt := range opts {
		opt(h)
	}
	return h
}

// observation is what Mender reports for the harness' current state: a pending
// commit for gateArtifact while it is staged, and nothing once it is committed
// or rolled back.
func (h *gateHarness) observation() mender.UpdateObservation {
	if h.committed == gateArtifact {
		return mender.UpdateObservation{
			State:             mender.StateNoUpdate,
			CommittedArtifact: gateArtifact,
			CommittedVersion:  gateVersion,
		}
	}
	return mender.UpdateObservation{
		State:             mender.StateNeedsCommit,
		PendingArtifact:   gateArtifact,
		PendingVersion:    gateVersion,
		CommittedArtifact: "release-" + gatePrevious,
		CommittedVersion:  gatePrevious,
	}
}

func allGateProbesPassing() []gateProbeResult {
	return []gateProbeResult{
		{Name: gateProbeUptime, Passed: true, Detail: "up to 10m, need 3m"},
		{Name: gateProbeSystemd, Passed: true, Detail: `systemd state "running"`},
		{Name: gateProbeUnits, Passed: true, Detail: "all required units active"},
		{Name: gateProbeVehicle, Passed: true, Detail: `vehicle state "stand-by"`},
		{Name: gateProbePowerManager, Passed: true, Detail: `power-manager state "running"`},
		{Name: gateProbeOTAStatus, Passed: true, Detail: "status is pending-reboot with no error"},
	}
}

func failingProbe(probes []gateProbeResult, name, detail string) []gateProbeResult {
	out := make([]gateProbeResult, len(probes))
	copy(out, probes)
	for i := range out {
		if out[i].Name == name {
			out[i].Passed = false
			out[i].Detail = detail
		}
	}
	return out
}

// markStatusPendingReboot puts the component in the state the gate expects to be
// gating from.
func (h *gateHarness) markStatusPendingReboot(t *testing.T) {
	t.Helper()
	if err := h.updater.status.SetPendingRebootForVersion(context.Background(), gateVersion); err != nil {
		t.Fatal(err)
	}
}

func (h *gateHarness) gated() *GatedCommit {
	return &GatedCommit{Artifact: gateArtifact, PendingVersion: gateVersion}
}

func (h *gateHarness) gateField(key string) string { return h.redis.HGet("ota", key) }

// A window whose probes all hold commits, clears its marker and says so.
func TestCommitGateCommitsWhenEveryProbeHolds(t *testing.T) {
	h := newGateHarness(t)
	h.markStatusPendingReboot(t)
	marker := commitgate.Marker{
		Artifact: gateArtifact, PendingVersion: gateVersion,
		BootID: gateBootA, FirstSeen: h.now, Verdict: commitgate.VerdictWaiting,
	}
	if err := h.store.Save(marker); err != nil {
		t.Fatal(err)
	}

	if done := h.updater.commitGateStep(&marker, h.gated()); !done {
		t.Fatal("a passing gate did not reach a verdict")
	}
	if h.commits != 1 {
		t.Fatalf("commit calls = %d, want 1", h.commits)
	}
	if h.rollbacks != 0 || h.reboots != 0 {
		t.Fatalf("passing gate rolled back %d times and rebooted %d times", h.rollbacks, h.reboots)
	}
	if _, err := h.store.Load(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker should be cleared after a commit, got err=%v", err)
	}
	if got := h.gateField("status:mdb"); got != "idle" {
		t.Errorf("status:mdb = %q, want idle", got)
	}
	if got := h.gateField("commit-gate:mdb"); got != gateStateCommitted {
		t.Errorf("commit-gate:mdb = %q, want %q", got, gateStateCommitted)
	}
}

// A window that is still short of its floor waits, and does not commit.
func TestCommitGateWaitsWhileAProbeFails(t *testing.T) {
	h := newGateHarness(t)
	h.markStatusPendingReboot(t)
	h.probes = failingProbe(allGateProbesPassing(), gateProbeUptime, "up to 1m0s, need 3m0s")
	marker := commitgate.Marker{
		Artifact: gateArtifact, PendingVersion: gateVersion,
		BootID: gateBootA, FirstSeen: h.now, Verdict: commitgate.VerdictWaiting,
	}

	if done := h.updater.commitGateStep(&marker, h.gated()); done {
		t.Fatal("a failing probe reached a verdict before the deadline")
	}
	if h.commits != 0 {
		t.Fatalf("commit calls = %d, want 0", h.commits)
	}
	if got := h.gateField("commit-gate:mdb"); got != gateStateWaiting {
		t.Errorf("commit-gate:mdb = %q, want %q", got, gateStateWaiting)
	}
	if got := h.gateField("commit-gate-reason:mdb"); !strings.Contains(got, "uptime") {
		t.Errorf("commit-gate-reason:mdb = %q, want the outstanding probe named", got)
	}
	deadlineWant := h.now.Add(config.DefaultCommitGateDeadline).UTC().Format(time.RFC3339)
	if got := h.gateField("commit-gate-deadline:mdb"); got != deadlineWant {
		t.Errorf("commit-gate-deadline:mdb = %q, want %q", got, deadlineWant)
	}

	// The probe recovers before the deadline: the same window still commits.
	h.probes = allGateProbesPassing()
	if done := h.updater.commitGateStep(&marker, h.gated()); !done {
		t.Fatal("a recovered probe did not reach a verdict")
	}
	if h.commits != 1 {
		t.Fatalf("commit calls = %d, want 1", h.commits)
	}
}

// The deadline fails the attempt closed: roll back, record it, quarantine, reboot.
func TestCommitGateDeadlineRollsBackAndReboots(t *testing.T) {
	h := newGateHarness(t)
	h.markStatusPendingReboot(t)
	h.probes = failingProbe(allGateProbesPassing(), gateProbeUnits, "librescoot-pm.service is not active")
	marker := commitgate.Marker{
		Artifact: gateArtifact, PendingVersion: gateVersion,
		BootID: gateBootA, FirstSeen: h.now, Verdict: commitgate.VerdictWaiting,
	}

	// Just short of the deadline: still waiting.
	h.now = h.now.Add(config.DefaultCommitGateDeadline - time.Second)
	if done := h.updater.commitGateStep(&marker, h.gated()); done {
		t.Fatal("the gate reached a verdict before its deadline")
	}

	// At the deadline: fail closed.
	h.now = h.now.Add(time.Second)
	if done := h.updater.commitGateStep(&marker, h.gated()); !done {
		t.Fatal("the gate did not reach a verdict at its deadline")
	}
	if h.commits != 0 {
		t.Fatalf("commit calls = %d, want 0", h.commits)
	}
	if h.rollbacks != 1 {
		t.Fatalf("rollback calls = %d, want 1", h.rollbacks)
	}
	if h.reboots != 1 {
		t.Fatalf("reboot requests = %d, want 1", h.reboots)
	}

	saved, err := h.store.Load()
	if err != nil {
		t.Fatalf("the rollback must leave its marker for the next boot: %v", err)
	}
	if !saved.RollbackAttempted {
		t.Error("RollbackAttempted was not recorded, so the next boot cannot tell a stuck rollback from a fresh attempt")
	}
	if saved.Verdict != commitgate.VerdictRolledBack {
		t.Errorf("marker verdict = %q, want %q", saved.Verdict, commitgate.VerdictRolledBack)
	}
	if !strings.Contains(saved.Reason, "librescoot-pm.service") {
		t.Errorf("marker reason = %q, want the failing probe named", saved.Reason)
	}
	if quarantined, err := h.store.Quarantined(gateArtifact); err != nil || !quarantined {
		t.Errorf("Quarantined(%s) = %v (err %v), want true", gateArtifact, quarantined, err)
	}
	if got := h.gateField("commit-gate:mdb"); got != gateStateRolledBack {
		t.Errorf("commit-gate:mdb = %q, want %q", got, gateStateRolledBack)
	}
	if got := h.gateField("error:mdb"); got != gateErrorRollback {
		t.Errorf("error:mdb = %q, want %q", got, gateErrorRollback)
	}
}

// The kill switch stops gating and lets the commit through, which is what
// "off" means.
func TestCommitGateDisabledAtRuntimeCommits(t *testing.T) {
	h := newGateHarness(t)
	h.markStatusPendingReboot(t)
	h.probes = failingProbe(allGateProbesPassing(), gateProbeUnits, "librescoot-pm.service is not active")
	h.updater.config.SetCommitGate(false)
	marker := commitgate.Marker{
		Artifact: gateArtifact, PendingVersion: gateVersion,
		BootID: gateBootA, FirstSeen: h.now, Verdict: commitgate.VerdictWaiting,
	}

	if done := h.updater.commitGateStep(&marker, h.gated()); !done {
		t.Fatal("a disabled gate did not reach a verdict")
	}
	if h.commits != 1 {
		t.Fatalf("commit calls = %d, want 1", h.commits)
	}
	if h.rollbacks != 0 {
		t.Fatalf("a disabled gate rolled back %d times", h.rollbacks)
	}
	if got := h.gateField("commit-gate:mdb"); got != gateStateDisabled {
		t.Errorf("commit-gate:mdb = %q, want %q", got, gateStateDisabled)
	}
}

// Reopening the marker for the same boot and artifact resumes the window
// unchanged, so a service restart cannot extend the deadline.
func TestOpenGateMarkerResumesWithoutExtendingTheDeadline(t *testing.T) {
	h := newGateHarness(t)
	firstSeen := h.now.Add(-19 * time.Minute)
	if err := h.store.Save(commitgate.Marker{
		Artifact: gateArtifact, PendingVersion: gateVersion,
		BootID: gateBootA, FirstSeen: firstSeen, Verdict: commitgate.VerdictWaiting,
	}); err != nil {
		t.Fatal(err)
	}

	marker, err := h.updater.openGateMarker(h.gated())
	if err != nil {
		t.Fatal(err)
	}
	if !marker.FirstSeen.Equal(firstSeen) {
		t.Fatalf("FirstSeen = %v, want the original %v", marker.FirstSeen, firstSeen)
	}

	// One minute later the deadline has passed, so the resumed window fails
	// closed rather than starting a fresh twenty minutes.
	h.now = h.now.Add(time.Minute)
	h.markStatusPendingReboot(t)
	h.probes = failingProbe(allGateProbesPassing(), gateProbeUnits, "librescoot-pm.service is not active")
	if done := h.updater.commitGateStep(&marker, h.gated()); !done {
		t.Fatal("the resumed window did not apply its original deadline")
	}
	if h.rollbacks != 1 {
		t.Fatalf("rollback calls = %d, want 1", h.rollbacks)
	}
}

// A fresh window records the boot it opened in and when it opened.
func TestOpenGateMarkerStartsAFreshWindow(t *testing.T) {
	h := newGateHarness(t)
	marker, err := h.updater.openGateMarker(h.gated())
	if err != nil {
		t.Fatal(err)
	}
	if marker.BootID != gateBootA {
		t.Errorf("BootID = %q, want %q", marker.BootID, gateBootA)
	}
	if !marker.FirstSeen.Equal(h.now) {
		t.Errorf("FirstSeen = %v, want %v", marker.FirstSeen, h.now)
	}
	if marker.Verdict != commitgate.VerdictWaiting {
		t.Errorf("Verdict = %q, want %q", marker.Verdict, commitgate.VerdictWaiting)
	}
	saved, err := h.store.Load()
	if err != nil {
		t.Fatalf("the opened window must be persisted: %v", err)
	}
	if saved.Artifact != gateArtifact {
		t.Errorf("persisted artifact = %q, want %q", saved.Artifact, gateArtifact)
	}
}

// Reconciliation opens a window instead of committing when the gate is enabled
// and the pending artifact is the one running.
func TestReconcileDefersTheCommitToTheGate(t *testing.T) {
	h := newGateHarness(t)
	rec, err := h.updater.ReconcilePendingUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if rec.NeedsReboot {
		t.Error("a running pending artifact does not need another reboot")
	}
	if rec.Gated == nil {
		t.Fatal("the gate did not take ownership of the commit")
	}
	if rec.Gated.Artifact != gateArtifact || rec.Gated.PendingVersion != gateVersion {
		t.Fatalf("gated commit = %+v, want the pending artifact", rec.Gated)
	}
	if h.commits != 0 {
		t.Fatalf("commit calls = %d, want 0: the gate owns the decision", h.commits)
	}
	if got := h.gateField("status:mdb"); got != "pending-reboot" {
		t.Errorf("status:mdb = %q, want pending-reboot", got)
	}
}

// With the gate off, the same state commits immediately and startup observes
// Mender exactly as it did before the gate existed.
func TestReconcileWithGateDisabledCommitsImmediately(t *testing.T) {
	h := newGateHarness(t)
	h.updater.config.SetCommitGate(false)
	observations := 0
	base := h.updater.observeUpdate
	h.updater.observeUpdate = func() (mender.UpdateObservation, error) {
		observations++
		return base()
	}

	rec, err := h.updater.ReconcilePendingUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if rec.Gated != nil || rec.NeedsReboot {
		t.Fatalf("reconciliation = %+v, want a settled immediate commit", rec)
	}
	if h.commits != 1 {
		t.Fatalf("commit calls = %d, want 1", h.commits)
	}
	if observations != 2 {
		t.Errorf("observe calls = %d, want 2 (before and after the commit)", observations)
	}
}

// A second boot presenting the same uncommitted artifact means the bootloader
// did not revert. With no rollback attempted yet, that rolls back once.
func TestSecondBootOnTheSameSlotRollsBackOnce(t *testing.T) {
	h := newGateHarness(t)
	h.bootID = gateBootB
	if err := h.store.Save(commitgate.Marker{
		Artifact: gateArtifact, PendingVersion: gateVersion,
		BootID: gateBootA, FirstSeen: h.now.Add(-time.Hour), Verdict: commitgate.VerdictWaiting,
	}); err != nil {
		t.Fatal(err)
	}

	rec, err := h.updater.ReconcilePendingUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if rec.Gated != nil || rec.NeedsReboot {
		t.Fatalf("reconciliation = %+v, want a settled rollback", rec)
	}
	if h.rollbacks != 1 {
		t.Fatalf("rollback calls = %d, want 1", h.rollbacks)
	}
	if h.reboots != 1 {
		t.Fatalf("reboot requests = %d, want 1", h.reboots)
	}
	if quarantined, err := h.store.Quarantined(gateArtifact); err != nil || !quarantined {
		t.Errorf("Quarantined(%s) = %v (err %v), want true", gateArtifact, quarantined, err)
	}
}

// A second boot after a rollback was already requested means the rollback never
// landed. Rebooting again would loop, so the gate stops and reports.
func TestSecondBootAfterAnAttemptedRollbackDoesNotRebootAgain(t *testing.T) {
	h := newGateHarness(t)
	h.bootID = gateBootB
	if err := h.store.Save(commitgate.Marker{
		Artifact: gateArtifact, PendingVersion: gateVersion,
		BootID: gateBootA, FirstSeen: h.now.Add(-time.Hour),
		RollbackAttempted: true, Verdict: commitgate.VerdictRolledBack,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := h.updater.ReconcilePendingUpdate(); err != nil {
		t.Fatal(err)
	}
	if h.reboots != 0 {
		t.Fatalf("reboot requests = %d, want 0: an unlanded rollback must not loop", h.reboots)
	}
	if h.rollbacks != 1 {
		t.Fatalf("rollback calls = %d, want 1", h.rollbacks)
	}
	if got := h.gateField("error:mdb"); got != gateErrorStuck {
		t.Errorf("error:mdb = %q, want %q", got, gateErrorStuck)
	}
	if got := h.gateField("commit-gate:mdb"); got != gateStateRollbackStuck {
		t.Errorf("commit-gate:mdb = %q, want %q", got, gateStateRollbackStuck)
	}
	if _, err := h.store.Load(); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the marker should be cleared so this cannot repeat, got err=%v", err)
	}
}

// A second boot on the committed slot means the bootloader reverted. Mender's
// pending state is stale, and the gate is the only thing on the MDB that can
// close it out.
func TestRevertedGateWindowFinalisesWithoutRebooting(t *testing.T) {
	h := newGateHarness(t)
	h.bootID = gateBootB
	h.running = gatePrevious
	if err := h.store.Save(commitgate.Marker{
		Artifact: gateArtifact, PendingVersion: gateVersion,
		BootID: gateBootA, FirstSeen: h.now.Add(-time.Hour), Verdict: commitgate.VerdictWaiting,
	}); err != nil {
		t.Fatal(err)
	}

	rec, err := h.updater.ReconcilePendingUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if rec.Gated != nil || rec.NeedsReboot {
		t.Fatalf("reconciliation = %+v, want a settled revert", rec)
	}
	if h.reboots != 0 {
		t.Fatalf("reboot requests = %d, want 0", h.reboots)
	}
	if h.rollbacks != 1 {
		t.Fatalf("rollback calls = %d, want 1: Mender's stale state must be cleared", h.rollbacks)
	}
	if got := h.gateField("status:mdb"); got != "idle" {
		t.Errorf("status:mdb = %q, want idle", got)
	}
	if got := h.gateField("commit-gate:mdb"); got != gateStateRolledBack {
		t.Errorf("commit-gate:mdb = %q, want %q", got, gateStateRolledBack)
	}
	if _, err := h.store.Load(); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("marker should be cleared after a revert, got err=%v", err)
	}
}

// A marker whose artifact Mender no longer holds describes an attempt that is
// not the current one, and is discarded rather than acted on.
func TestStaleMarkerForAnotherArtifactIsDiscarded(t *testing.T) {
	h := newGateHarness(t)
	h.updater.config.SetCommitGate(false)
	if err := h.store.Save(commitgate.Marker{
		Artifact: "release-nightly-20260920T003447", PendingVersion: "nightly-20260920t003447",
		BootID: gateBootB, FirstSeen: h.now.Add(-time.Hour), Verdict: commitgate.VerdictWaiting,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := h.updater.ReconcilePendingUpdate(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.Load(); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the stale marker should be discarded, got err=%v", err)
	}
	if h.rollbacks != 0 || h.reboots != 0 {
		t.Fatalf("a stale marker must not roll back (%d) or reboot (%d)", h.rollbacks, h.reboots)
	}
}

// A commit that fails is a hard failure, not a silent success.
func TestCommitGateCommitFailureRollsBack(t *testing.T) {
	h := newGateHarness(t)
	h.markStatusPendingReboot(t)
	commitErr := errors.New("mender refused the commit")
	h.updater.commitUpdate = func() error { return commitErr }
	marker := commitgate.Marker{
		Artifact: gateArtifact, PendingVersion: gateVersion,
		BootID: gateBootA, FirstSeen: h.now, Verdict: commitgate.VerdictWaiting,
	}

	if done := h.updater.commitGateStep(&marker, h.gated()); !done {
		t.Fatal("a failed commit did not reach a verdict")
	}
	if h.rollbacks != 1 {
		t.Fatalf("rollback calls = %d, want 1", h.rollbacks)
	}
	if h.reboots != 1 {
		t.Fatalf("reboot requests = %d, want 1", h.reboots)
	}
}

// A window whose artifact Mender replaced is abandoned: the decision is no
// longer this gate's to make.
func TestCommitGateAbandonsWhenMenderMovesOn(t *testing.T) {
	h := newGateHarness(t)
	h.markStatusPendingReboot(t)
	h.updater.observeUpdate = func() (mender.UpdateObservation, error) {
		return mender.UpdateObservation{
			State:             mender.StateNoUpdate,
			CommittedArtifact: "release-nightly-20260922T003447",
			CommittedVersion:  "nightly-20260922t003447",
		}, nil
	}
	marker := commitgate.Marker{
		Artifact: gateArtifact, PendingVersion: gateVersion,
		BootID: gateBootA, FirstSeen: h.now, Verdict: commitgate.VerdictWaiting,
	}

	if done := h.updater.commitGateStep(&marker, h.gated()); !done {
		t.Fatal("an abandoned window did not reach a verdict")
	}
	if h.commits != 0 || h.rollbacks != 0 || h.reboots != 0 {
		t.Fatalf("abandoning must not commit (%d), roll back (%d) or reboot (%d)", h.commits, h.rollbacks, h.reboots)
	}
	if got := h.gateField("commit-gate:mdb"); got != gateStateAbandoned {
		t.Errorf("commit-gate:mdb = %q, want %q", got, gateStateAbandoned)
	}
	if _, err := h.store.Load(); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("marker should be cleared when the window is abandoned, got err=%v", err)
	}
}

// The gate cannot open a window without a marker, and rather than commit blind
// or reboot into a possible loop, it holds and reports.
func TestCommitGateHoldsWhenTheMarkerCannotBeWritten(t *testing.T) {
	h := newGateHarness(t)
	h.markStatusPendingReboot(t)
	// A marker path whose parent is a file cannot be written.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.updater.gateStore = commitgate.NewStoreAt(
		filepath.Join(blocker, "commit-gate-mdb.json"),
		filepath.Join(blocker, "quarantine.json"),
	)

	h.updater.runCommitGate(h.gated())

	if h.commits != 0 {
		t.Fatalf("commit calls = %d, want 0: the gate could not record its decision", h.commits)
	}
	if h.reboots != 0 {
		t.Fatalf("reboot requests = %d, want 0", h.reboots)
	}
	if got := h.gateField("error:mdb"); got != gateErrorHeld {
		t.Errorf("error:mdb = %q, want %q", got, gateErrorHeld)
	}
	if got := h.gateField("commit-gate:mdb"); got != gateErrorHeld {
		t.Errorf("commit-gate:mdb = %q, want %q", got, gateErrorHeld)
	}
}

// A quarantined version is not a staged install target.
func TestQuarantineBlocksTheStagedReinstall(t *testing.T) {
	h := newGateHarness(t)
	if err := h.store.AddQuarantine(gateArtifact); err != nil {
		t.Fatal(err)
	}

	rejected := h.updater.gateQuarantinedVersions()
	if !rejected[gateVersion] {
		t.Fatalf("quarantined versions = %v, want %s", rejected, gateVersion)
	}

	// And a release offering that version is not selected.
	releases := []Release{
		{TagName: gateVersion},
		{TagName: gatePrevious},
	}
	filtered := h.updater.withoutGateRejectedReleases(releases)
	if len(filtered) != 1 || filtered[0].TagName != gatePrevious {
		t.Fatalf("filtered releases = %+v, want only the older one", filtered)
	}
}

// A newer release is unaffected: the quarantine names one artifact, not a channel.
func TestQuarantineDoesNotBlockANewerRelease(t *testing.T) {
	h := newGateHarness(t)
	if err := h.store.AddQuarantine(gateArtifact); err != nil {
		t.Fatal(err)
	}

	releases := []Release{
		{TagName: "nightly-20260922t003447"},
		{TagName: gateVersion},
	}
	filtered := h.updater.withoutGateRejectedReleases(releases)
	if len(filtered) != 1 || filtered[0].TagName != "nightly-20260922t003447" {
		t.Fatalf("filtered releases = %+v, want the newer release kept", filtered)
	}
}

// Quarantine entries are pruned only once the vehicle has moved past them, and
// an entry at the running version is kept: after a rollback the rejected artifact
// is exactly the version above the one now running, and after a stuck rollback it
// is the version running.
func TestPruneCommitGateQuarantineKeepsReachableEntries(t *testing.T) {
	h := newGateHarness(t)
	for _, artifact := range []string{"release-nightly-20260915T003447", gateArtifact, "release-nightly-20260922T003447"} {
		if err := h.store.AddQuarantine(artifact); err != nil {
			t.Fatal(err)
		}
	}
	// The vehicle rolled back to the previous version.
	h.running = gatePrevious
	h.updater.pruneCommitGateQuarantine()

	for artifact, want := range map[string]bool{
		"release-nightly-20260915T003447": false, // behind the running version
		gateArtifact:                      true,  // the rejected artifact, still ahead
		"release-nightly-20260922T003447": true,  // newer than the running version
	} {
		got, err := h.store.Quarantined(artifact)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("Quarantined(%s) = %v, want %v", artifact, got, want)
		}
	}
}

// A stuck rollback leaves the rejected artifact running; its quarantine entry
// must survive the prune that runs at the next startup.
func TestPruneCommitGateQuarantineKeepsTheRunningVersion(t *testing.T) {
	h := newGateHarness(t)
	if err := h.store.AddQuarantine(gateArtifact); err != nil {
		t.Fatal(err)
	}
	h.running = gateVersion
	h.updater.pruneCommitGateQuarantine()

	quarantined, err := h.store.Quarantined(gateArtifact)
	if err != nil {
		t.Fatal(err)
	}
	if !quarantined {
		t.Error("the artifact that is running after a stuck rollback must stay quarantined")
	}
}

// Probe evaluation maps each observation onto the probe that consumes it.
func TestEvaluateCommitGateProbes(t *testing.T) {
	h := newGateHarness(t)
	cfg := h.updater.config.CommitGateSettings()

	results := h.updater.evaluateCommitGateProbes(cfg)
	passes := probePassMap(results)
	for _, name := range []string{gateProbeUptime, gateProbeSystemd, gateProbeUnits, gateProbeVehicle, gateProbePowerManager, gateProbeOTAStatus} {
		if _, ok := passes[name]; !ok {
			t.Errorf("probe %s was not evaluated", name)
		}
	}
	// /proc/uptime on a test host is past any sane floor, and systemd on a
	// developer machine is not the gate's business, so only the Redis-backed
	// probes are asserted here.
	if passes[gateProbeOTAStatus] {
		t.Error("ota-status passed with no pending-reboot status in Redis")
	}
	if passes[gateProbeVehicle] {
		t.Error("vehicle passed with no vehicle state in Redis")
	}
}

func TestFirstFailingProbeReportsThePriorityOrder(t *testing.T) {
	probes := []gateProbeResult{
		{Name: "a", Passed: true},
		{Name: "b", Passed: false, Detail: "b failed"},
		{Name: "c", Passed: false, Detail: "c failed"},
	}
	name, detail := firstFailingProbe(probes)
	if name != "b" || detail != "b failed" {
		t.Fatalf("firstFailingProbe = (%q, %q), want the first failure", name, detail)
	}
	if name, _ := firstFailingProbe([]gateProbeResult{{Name: "a", Passed: true}}); name != "" {
		t.Fatalf("firstFailingProbe on a passing set = %q, want empty", name)
	}
}

// systemdRunning accepts the states in which boot has finished, and "degraded"
// is one of them: the required-unit probe decides whether the failures matter.
func TestSystemdRunningStates(t *testing.T) {
	for state, want := range map[string]bool{
		"running":     true,
		"degraded":    true,
		"starting":    false,
		"maintenance": false,
		"offline":     false,
		"unknown":     false,
		"":            false,
	} {
		if got := systemdRunning(state); got != want {
			t.Errorf("systemdRunning(%q) = %v, want %v", state, got, want)
		}
	}
}

// The gate must be observably open while it evaluates, so a concurrent install
// command can be refused before it writes a status the gate would read as
// evidence about the image under test.
func TestGateWindowIsOpenOnlyWhileTheGateRuns(t *testing.T) {
	h := newGateHarness(t)
	h.markStatusPendingReboot(t)

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	h.updater.gateEvaluate = func(config.CommitGateSettings) []gateProbeResult {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return allGateProbesPassing()
	}

	if h.updater.gateWindowIsOpen() {
		t.Fatal("the window must not be open before the gate starts")
	}

	h.updater.startCommitGate(h.gated())
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the gate never evaluated its probes")
	}

	if !h.updater.gateWindowIsOpen() {
		t.Error("the window must be open while the gate evaluates")
	}

	close(release)
	h.updater.wg.Wait()

	if h.updater.gateWindowIsOpen() {
		t.Error("the window must close with the gate")
	}
	if h.commits != 1 {
		t.Fatalf("commit calls = %d, want 1", h.commits)
	}
}

// The window flag is read by the command handlers and written by the gate
// goroutine, so it is guarded rather than a plain field.
func TestGateWindowFlagIsGuarded(t *testing.T) {
	h := newGateHarness(t)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			h.updater.setGateWindowOpen(i%2 == 0)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = h.updater.gateWindowIsOpen()
		}
	}()
	wg.Wait()
}

// The units probe must accept a oneshot that has done its job and left, and
// must not accept one that never ran: on a healthy device the version service
// is Type=oneshot with RemainAfterExit=no, so it reports inactive forever after
// a successful run, while systemd reports Result=success even for a unit that
// has never been invoked at all.
func TestUnitRanStateDistinguishesRanFromNeverRan(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   bool
	}{
		{
			name:   "oneshot ran and exited cleanly",
			output: "InvocationID=fdaf38e462944333bceaf6f160f8d9a4\nResult=success\nExecMainStatus=0",
			want:   true,
		},
		{
			name:   "never invoked, empty property",
			output: "InvocationID=\nResult=success\nExecMainStatus=0",
			want:   false,
		},
		{
			name:   "never invoked, property omitted",
			output: "Result=success\nExecMainStatus=0",
			want:   false,
		},
		{
			name:   "invoked but failed",
			output: "InvocationID=fdaf38e462944333bceaf6f160f8d9a4\nResult=exit-code\nExecMainStatus=1",
			want:   false,
		},
		{
			name:   "invoked, zero status, non-success result",
			output: "InvocationID=fdaf38e462944333bceaf6f160f8d9a4\nResult=start-limit-hit\nExecMainStatus=0",
			want:   false,
		},
		{
			name:   "empty output",
			output: "",
			want:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := unitRanState(tc.output); got != tc.want {
				t.Errorf("unitRanState(%q) = %v, want %v", tc.output, got, tc.want)
			}
		})
	}
}

// The failing probe names the unit, in whichever of the two states it was in.
func TestGateProbeUnitsReportsTheOffendingUnit(t *testing.T) {
	h := newGateHarness(t)
	// On a developer host the required units do not exist at all, which is the
	// same as never having run: the probe must name the unit rather than fail.
	inactive, err := h.updater.gateProbeUnits([]string{"librescoot-gate-test-nonexistent.service"})
	if err != nil {
		t.Fatalf("gateProbeUnits: %v", err)
	}
	if inactive != "librescoot-gate-test-nonexistent.service" {
		t.Errorf("gateProbeUnits = %q, want the offending unit", inactive)
	}

	// An empty list satisfies the probe.
	inactive, err = h.updater.gateProbeUnits(nil)
	if err != nil || inactive != "" {
		t.Errorf("gateProbeUnits(nil) = (%q, %v), want no failure", inactive, err)
	}
}

// saveActivation writes the record triggerDBCLocalReboot leaves behind before
// asking the DBC to reboot into the installed image.
func (h *gateHarness) saveActivation(t *testing.T) {
	t.Helper()
	if err := dbcstate.SaveActivationAttempt(h.updater.activationAttempt, dbcstate.ActivationAttempt{
		Artifact: gateArtifact,
		BootID:   gateBootA,
	}); err != nil {
		t.Fatal(err)
	}
}

func (h *gateHarness) activationOpen() bool {
	_, err := dbcstate.LoadActivationAttempt(h.updater.activationAttempt)
	return err == nil
}

func (h *gateHarness) pushedCommands() []string {
	commands, err := h.updater.redis.GetClient().Raw().LRange(context.Background(), "scooter:update", 0, -1).Result()
	if err != nil {
		return nil
	}
	return commands
}

// The probe set follows the component: the MDB gates on its own power state,
// the DBC does not read it at all, because the hash belongs to the MDB there.
func TestCommitGateProbesAreComponentAware(t *testing.T) {
	mdb := newGateHarness(t)
	mdbProbes := probePassMap(mdb.updater.evaluateCommitGateProbes(mdb.updater.config.CommitGateSettings()))
	if _, ok := mdbProbes[gateProbePowerManager]; !ok {
		t.Error("the MDB must gate on its own power-manager state")
	}

	dbc := newGateHarnessFor(t, "dbc")
	dbcProbes := probePassMap(dbc.updater.evaluateCommitGateProbes(dbc.updater.config.CommitGateSettings()))
	if _, ok := dbcProbes[gateProbePowerManager]; ok {
		t.Error("the DBC must not gate on the MDB's power-manager state")
	}
	for _, name := range []string{gateProbeUptime, gateProbeSystemd, gateProbeUnits, gateProbeVehicle, gateProbeOTAStatus} {
		if _, ok := dbcProbes[name]; !ok {
			t.Errorf("the DBC must evaluate %s", name)
		}
	}
}

// The DBC gets a window like the MDB, and the activation attempt it asked for is
// still open while the gate decides.
func TestReconcileOpensAGateWindowOnTheDBC(t *testing.T) {
	h := newGateHarnessFor(t, "dbc")
	h.saveActivation(t)

	rec, err := h.updater.ReconcilePendingUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if rec.NeedsReboot || rec.Gated == nil {
		t.Fatalf("reconciliation = %+v, want a gated commit", rec)
	}
	if h.commits != 0 {
		t.Fatalf("commit calls = %d, want 0: the gate owns the decision", h.commits)
	}
	if got := h.redis.HGet("ota", "status:dbc"); got != "pending-reboot" {
		t.Errorf("status:dbc = %q, want pending-reboot", got)
	}
	if !h.activationOpen() {
		t.Error("the activation attempt must stay open while the gate decides")
	}
	// reconstructPendingState restores the DBC lifecycle for the window.
	if !slices.Contains(h.pushedCommands(), "start-dbc") {
		t.Errorf("pushed commands = %v, want start-dbc restored", h.pushedCommands())
	}
}

// A DBC window that earns its commit completes the lifecycle exactly as an
// ungated startup commit does: activation attempt closed, status idle, and
// complete-dbc handed to vehicle-service.
func TestDBCGateCommitCompletesTheLifecycle(t *testing.T) {
	h := newGateHarnessFor(t, "dbc")
	h.saveActivation(t)
	h.markStatusPendingReboot(t)
	marker := commitgate.Marker{
		Artifact: gateArtifact, PendingVersion: gateVersion,
		BootID: gateBootB, FirstSeen: h.now, Verdict: commitgate.VerdictWaiting,
	}

	if done := h.updater.commitGateStep(&marker, h.gated()); !done {
		t.Fatal("a passing DBC window did not reach a verdict")
	}
	if h.commits != 1 {
		t.Fatalf("commit calls = %d, want 1", h.commits)
	}
	if h.activationOpen() {
		t.Error("the activation attempt must be closed by a verified commit")
	}
	if got := h.redis.HGet("ota", "status:dbc"); got != "idle" {
		t.Errorf("status:dbc = %q, want idle", got)
	}
	if !slices.Contains(h.pushedCommands(), "complete-dbc") {
		t.Errorf("pushed commands = %v, want complete-dbc", h.pushedCommands())
	}
	if _, err := h.store.Load(); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("marker should be cleared after a commit, got err=%v", err)
	}
	if quarantined, err := h.store.Quarantined(gateArtifact); err != nil || quarantined {
		t.Errorf("a committed artifact must not be quarantined, got %v (err %v)", quarantined, err)
	}
}

// A DBC window the bootloader reverted: Mender's commit-pending state is stale,
// and both halves of the record — the gate marker and the activation attempt —
// have to close.
func TestRevertedGateWindowOnTheDBCClosesTheActivationAttempt(t *testing.T) {
	h := newGateHarnessFor(t, "dbc")
	h.bootID = gateBootB
	h.running = gatePrevious
	h.saveActivation(t)
	if err := h.store.Save(commitgate.Marker{
		Artifact: gateArtifact, PendingVersion: gateVersion,
		BootID: gateBootA, FirstSeen: h.now.Add(-time.Hour), Verdict: commitgate.VerdictWaiting,
	}); err != nil {
		t.Fatal(err)
	}

	rec, err := h.updater.ReconcilePendingUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if rec.Gated != nil || rec.NeedsReboot {
		t.Fatalf("reconciliation = %+v, want a settled revert", rec)
	}
	if h.reboots != 0 {
		t.Errorf("reboot requests = %d, want 0", h.reboots)
	}
	if h.rollbacks != 1 {
		t.Errorf("rollback calls = %d, want 1", h.rollbacks)
	}
	if h.activationOpen() {
		t.Error("the activation attempt must be closed by a revert")
	}
	if _, err := h.store.Load(); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("gate marker should be cleared, got err=%v", err)
	}
	if got := h.redis.HGet("ota", "status:dbc"); got != "idle" {
		t.Errorf("status:dbc = %q, want idle", got)
	}
	if quarantined, err := h.store.Quarantined(gateArtifact); err != nil || !quarantined {
		t.Errorf("the reverted artifact should be quarantined, got %v (err %v)", quarantined, err)
	}
}

// A second boot still on the pending artifact after a requested rollback: the
// DBC activation record has to go too, or it would outlive the gate's decision
// and never be examined again.
func TestStuckGateOnTheDBCClosesTheActivationAttempt(t *testing.T) {
	h := newGateHarnessFor(t, "dbc")
	h.bootID = gateBootB
	h.saveActivation(t)
	if err := h.store.Save(commitgate.Marker{
		Artifact: gateArtifact, PendingVersion: gateVersion,
		BootID: gateBootA, FirstSeen: h.now.Add(-time.Hour),
		RollbackAttempted: true, Verdict: commitgate.VerdictRolledBack,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := h.updater.ReconcilePendingUpdate(); err != nil {
		t.Fatal(err)
	}
	if h.reboots != 0 {
		t.Errorf("reboot requests = %d, want 0: an unlanded rollback must not loop", h.reboots)
	}
	if h.rollbacks != 1 {
		t.Errorf("rollback calls = %d, want 1", h.rollbacks)
	}
	if h.activationOpen() {
		t.Error("the activation attempt must be closed when the gate holds")
	}
	if got := h.redis.HGet("ota", "error:dbc"); got != gateErrorStuck {
		t.Errorf("error:dbc = %q, want %q", got, gateErrorStuck)
	}
	if _, err := h.store.Load(); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("gate marker should be cleared, got err=%v", err)
	}
}

// The DBC requires only its own units: the MDB's services cannot be required
// from here, and the DBC's valkey server is disabled.
func TestCommitGateUnitsAreComponentScoped(t *testing.T) {
	dbc := newGateHarnessFor(t, "dbc")
	units := dbc.updater.config.CommitGateSettings().RequiredUnits
	want := []string{"librescoot-version.service", "dbc-dispatcher.service"}
	if !slices.Equal(units, want) {
		t.Errorf("dbc required units = %v, want %v", units, want)
	}
	for _, forbidden := range []string{"librescoot-vehicle.service", "librescoot-pm.service", "librescoot-settings.service", "valkey.service"} {
		if slices.Contains(units, forbidden) {
			t.Errorf("dbc required units must not include %s", forbidden)
		}
	}
}
