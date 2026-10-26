package updater

import (
	"context"
	"errors"
	"testing"

	"github.com/librescoot/update-service/internal/commitgate"
	"github.com/librescoot/update-service/internal/mender"
)

func TestBootMaintenanceWaitsForVerifiedCommit(t *testing.T) {
	for _, component := range []string{"mdb", "dbc"} {
		for _, disabled := range []bool{false, true} {
			name := component
			if disabled {
				name += "/disabled-gate"
			}
			t.Run(name, func(t *testing.T) {
				h := newGateHarnessFor(t, component)
				h.markStatusPendingReboot(t)
				f := newFakeBoot()
				d := f.deps()
				d.rootfsState = func() (mender.UpdateState, error) { return h.observation().State, nil }
				calls := 0
				h.updater.localBootUpdate = func() {
					calls++
					if h.commits != 1 || h.gateField("status:"+component) != "idle" {
						t.Fatal("boot maintenance started before verified rootfs commit")
					}
					if applied, err := runLocalBootUpdate(context.Background(), component, d); err != nil || !applied {
						t.Fatalf("boot retry applied=%v err=%v", applied, err)
					}
				}
				h.updater.maintainLocalBoot(true)
				if calls != 0 || len(f.events) != 0 {
					t.Fatal("startup touched boot assets while rootfs was pending")
				}
				marker := commitgate.Marker{Artifact: gateArtifact, PendingVersion: gateVersion,
					BootID: gateBootA, FirstSeen: h.now, Verdict: commitgate.VerdictWaiting}
				h.probes = failingProbe(allGateProbesPassing(), gateProbeUptime, "waiting for floor")
				if done := h.updater.commitGateStep(&marker, h.gated()); done || calls != 0 {
					t.Fatal("boot maintenance ran while the health gate was waiting")
				}
				if disabled {
					h.updater.config.SetCommitGate(false)
				} else {
					h.probes = allGateProbesPassing()
				}
				if done := h.updater.commitGateStep(&marker, h.gated()); !done || calls != 1 || f.writes != 1 || f.held {
					t.Fatalf("done=%v calls=%d writes=%d held=%v", done, calls, f.writes, f.held)
				}
			})
		}
	}
}

func TestBootMaintenanceDoesNotFollowFailedGate(t *testing.T) {
	for _, mode := range []string{"commit-error", "unverified-commit", "rollback", "abandoned"} {
		t.Run(mode, func(t *testing.T) {
			h := newGateHarness(t)
			h.markStatusPendingReboot(t)
			h.updater.localBootUpdate = func() { t.Fatal("boot maintenance ran without a verified commit") }
			marker := commitgate.Marker{Artifact: gateArtifact, PendingVersion: gateVersion,
				BootID: gateBootA, FirstSeen: h.now, Verdict: commitgate.VerdictWaiting}
			switch mode {
			case "commit-error":
				h.updater.commitUpdate = func() error { return errors.New("commit failed") }
			case "unverified-commit":
				h.updater.commitUpdate = func() error { return nil }
			case "rollback":
				h.probes = failingProbe(allGateProbesPassing(), gateProbeUnits, "unit failed")
				marker.Waiting = h.updater.config.CommitGateSettings().Deadline
			case "abandoned":
				h.updater.observeUpdate = func() (mender.UpdateObservation, error) {
					return mender.UpdateObservation{State: mender.StateNoUpdate}, nil
				}
			}
			if done := h.updater.commitGateStep(&marker, h.gated()); !done {
				t.Fatal("gate did not reach a verdict")
			}
		})
	}
}

func TestBootMaintenanceRunsAtSettledStartup(t *testing.T) {
	calls := 0
	u := &Updater{localBootUpdate: func() { calls++ }}
	u.maintainLocalBoot(false)
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
}
