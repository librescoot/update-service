package updater

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/librescoot/update-service/internal/commitgate"
	"github.com/librescoot/update-service/internal/dbcstate"
	"github.com/librescoot/update-service/internal/mender"
	"github.com/librescoot/update-service/internal/status"
)

type activationHarness struct {
	u           *Updater
	observation mender.UpdateObservation
	boot        mender.BootState
	origin      mender.RootfsOrigin
	id          string
	running     string
	commits     int
	rollbacks   int
}

func newActivationHarness(t *testing.T, component string) *activationHarness {
	t.Helper()
	u, _ := newPendingCommitUpdater(t)
	u.config.Component = component
	u.config.SetCommitGate(false)
	u.status = status.NewReporter(u.redis.GetClient(), component, u.logger)
	h := &activationHarness{u: u, id: "boot-new", running: "v1.3.1",
		observation: mender.UpdateObservation{State: mender.StateNeedsCommit, PendingArtifact: "release-v1.4.0", PendingVersion: "v1.4.0", CommittedArtifact: "release-v1.3.1", CommittedVersion: "v1.3.1"},
		boot:        mender.BootState{RunningSlot: "slot-a", OtherSlot: "slot-b", SelectedSlot: "slot-a"},
		origin:      mender.RootfsOrigin{OriginalSlot: "slot-a", Artifact: "release-v1.4.0", CommittedArtifact: "release-v1.3.1"},
	}
	u.observeUpdate = func() (mender.UpdateObservation, error) { return h.observation, nil }
	u.bootState = func() (mender.BootState, error) { return h.boot, nil }
	u.rootfsOrigin = func() (mender.RootfsOrigin, error) { return h.origin, nil }
	u.bootID = func() (string, error) { return h.id, nil }
	u.runningVersion = func() (string, error) { return h.running, nil }
	u.artifactInfo = func(string) (*mender.ArtifactInfo, error) {
		return &mender.ArtifactInfo{ArtifactName: h.origin.Artifact}, nil
	}
	u.rollbackUpdate = func() error {
		h.rollbacks++
		h.observation = mender.UpdateObservation{State: mender.StateNoUpdate, CommittedArtifact: h.origin.CommittedArtifact, CommittedVersion: mender.VersionFromArtifact(h.origin.CommittedArtifact)}
		return nil
	}
	u.commitUpdate = func() error {
		h.commits++
		h.observation = mender.UpdateObservation{State: mender.StateNoUpdate, CommittedArtifact: h.origin.Artifact, CommittedVersion: mender.VersionFromArtifact(h.origin.Artifact)}
		h.boot.UpgradeAvailable = false
		return nil
	}
	return h
}

func (h *activationHarness) attempt(t *testing.T, phase string) dbcstate.ActivationAttempt {
	t.Helper()
	a := dbcstate.ActivationAttempt{Schema: 1, AttemptID: "test-attempt", Component: h.u.config.Component, Artifact: h.origin.Artifact, CommittedArtifact: h.origin.CommittedArtifact, BootID: "boot-install", OriginalSlot: "slot-a", TargetSlot: "slot-b", Phase: phase}
	if err := dbcstate.SaveActivationAttempt(h.u.activationAttempt, a); err != nil {
		t.Fatal(err)
	}
	return a
}

func (h *activationHarness) pendingReboot() {
	h.id = "boot-install"
	h.boot.SelectedSlot = "slot-b"
	h.boot.UpgradeAvailable = true
}

func (h *activationHarness) trialBoot() {
	h.running = "v1.4.0"
	h.boot = mender.BootState{RunningSlot: "slot-b", OtherSlot: "slot-a", SelectedSlot: "slot-b", UpgradeAvailable: true}
}

func TestActivationRecoveryBothComponents(t *testing.T) {
	for _, component := range []string{"mdb", "dbc"} {
		for _, scenario := range []string{"same-boot", "trial-boot", "fallback", "legacy-fallback", "legacy-trial", "no-marker-fallback"} {
			t.Run(component+"/"+scenario, func(t *testing.T) {
				h := newActivationHarness(t, component)
				switch scenario {
				case "same-boot":
					h.attempt(t, "staged")
					h.pendingReboot()
				case "trial-boot":
					h.attempt(t, "staged")
					h.trialBoot()
				case "fallback":
					h.attempt(t, "staged")
				case "legacy-fallback", "legacy-trial":
					if err := dbcstate.SaveActivationAttempt(h.u.activationAttempt, dbcstate.ActivationAttempt{Artifact: h.origin.Artifact, BootID: "boot-install"}); err != nil {
						t.Fatal(err)
					}
					if scenario == "legacy-trial" {
						h.trialBoot()
					}
				}
				rec, err := h.u.ReconcilePendingUpdate()
				if err != nil {
					t.Fatal(err)
				}
				switch scenario {
				case "same-boot":
					if !rec.NeedsReboot || h.commits != 0 || h.rollbacks != 0 {
						t.Fatalf("unexpected actions: %+v, commit=%d rollback=%d", rec, h.commits, h.rollbacks)
					}
					if _, err := dbcstate.LoadActivationAttempt(h.u.activationAttempt); err != nil {
						t.Fatal("lost pre-reboot evidence", err)
					}
				case "trial-boot", "legacy-trial":
					if rec.NeedsReboot || h.commits != 1 || h.rollbacks != 0 {
						t.Fatalf("unexpected commit actions %+v %d %d", rec, h.commits, h.rollbacks)
					}
					if _, err := dbcstate.LoadActivationAttempt(h.u.activationAttempt); !os.IsNotExist(err) {
						t.Fatal("commit marker not closed", err)
					}
				default:
					if rec.NeedsReboot || h.rollbacks != 1 || h.commits != 0 {
						t.Fatalf("unexpected fallback actions %+v %d %d", rec, h.commits, h.rollbacks)
					}
					last, err := dbcstate.LoadActivationAttempt(h.u.activationAttempt + ".last")
					if err != nil || last.Phase != "rolled-back" || last.ObservedBootID != h.id {
						t.Fatalf("outcome=%+v err=%v", last, err)
					}
					if scenario == "no-marker-fallback" && last.BootID != "" {
						t.Fatal("fabricated legacy originating boot ID")
					}
					rejected, err := h.u.gateStore.Quarantined(h.origin.Artifact)
					if err != nil || !rejected {
						t.Fatal("failed artifact not quarantined", err)
					}
					if _, err := dbcstate.LoadActivationAttempt(h.u.activationAttempt); !os.IsNotExist(err) {
						t.Fatal("fallback marker not closed", err)
					}
					got, _ := h.u.status.GetStatus(h.u.ctx)
					if got != status.StatusError {
						t.Fatalf("failed activation reported %s", got)
					}
				}
			})
		}
	}
}

func TestActivationRejectsAmbiguousEvidence(t *testing.T) {
	cases := map[string]func(*activationHarness){
		"new boot without fallback":     func(h *activationHarness) { h.boot.SelectedSlot = "slot-b"; h.boot.UpgradeAvailable = true },
		"same boot fallback":            func(h *activationHarness) { h.id = "boot-install" },
		"missing upgrade flag on trial": func(h *activationHarness) { h.trialBoot(); h.boot.UpgradeAvailable = false },
		"wrong module origin":           func(h *activationHarness) { h.origin.OriginalSlot = "slot-b" },
		"wrong module identity":         func(h *activationHarness) { h.origin.Artifact = "release-other" },
		"wrong committed identity":      func(h *activationHarness) { h.observation.CommittedArtifact = "release-other" },
		"wrong running image":           func(h *activationHarness) { h.running = "v1.2.0" },
		"unreadable bootenv": func(h *activationHarness) {
			h.u.bootState = func() (mender.BootState, error) { return mender.BootState{}, errors.New("bootenv unavailable") }
		},
		"missing module origin": func(h *activationHarness) {
			h.u.rootfsOrigin = func() (mender.RootfsOrigin, error) { return mender.RootfsOrigin{}, os.ErrNotExist }
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			h := newActivationHarness(t, "mdb")
			h.attempt(t, "staged")
			change(h)
			if _, err := h.u.ReconcilePendingUpdate(); err == nil {
				t.Fatal("accepted ambiguous evidence")
			}
			if h.rollbacks != 0 || h.commits != 0 {
				t.Fatal("destructive action on ambiguous evidence")
			}
			if _, err := dbcstate.LoadActivationAttempt(h.u.activationAttempt); err != nil {
				t.Fatal("lost evidence", err)
			}
		})
	}
}

func TestActivationRollbackRequiresVerifiedResult(t *testing.T) {
	cases := map[string]func(*activationHarness){
		"command error": func(h *activationHarness) {
			h.u.rollbackUpdate = func() error { h.rollbacks++; return errors.New("rollback failed") }
		},
		"pending survives": func(h *activationHarness) { h.u.rollbackUpdate = func() error { h.rollbacks++; return nil } },
		"wrong next slot": func(h *activationHarness) {
			orig := h.u.rollbackUpdate
			h.u.rollbackUpdate = func() error { _ = orig(); h.boot.SelectedSlot = "slot-b"; return nil }
		},
		"upgrade flag survives": func(h *activationHarness) {
			orig := h.u.rollbackUpdate
			h.u.rollbackUpdate = func() error { _ = orig(); h.boot.UpgradeAvailable = true; return nil }
		},
		"wrong committed artifact": func(h *activationHarness) {
			orig := h.u.rollbackUpdate
			h.u.rollbackUpdate = func() error { _ = orig(); h.observation.CommittedArtifact = "release-other"; return nil }
		},
		"quarantine write fails": func(h *activationHarness) {
			blocked := filepath.Join(t.TempDir(), "file")
			if err := os.WriteFile(blocked, []byte("x"), 0600); err != nil {
				t.Fatal(err)
			}
			h.u.gateStore = commitgate.NewStoreAt(filepath.Join(blocked, "gate"), filepath.Join(blocked, "quarantine"))
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			h := newActivationHarness(t, "mdb")
			h.attempt(t, "staged")
			change(h)
			if _, err := h.u.ReconcilePendingUpdate(); err == nil {
				t.Fatal("accepted unverified rollback")
			}
			if h.commits != 0 {
				t.Fatal("committed failed activation")
			}
			if _, err := dbcstate.LoadActivationAttempt(h.u.activationAttempt); err != nil {
				t.Fatal("lost failed rollback evidence", err)
			}
			got, _ := h.u.status.GetStatus(h.u.ctx)
			if got != status.StatusError {
				t.Fatalf("reported %s", got)
			}
		})
	}
}

func TestActivationInstallIntentPrecedesEveryInstall(t *testing.T) {
	for _, component := range []string{"mdb", "dbc"} {
		t.Run(component, func(t *testing.T) {
			var events []string
			u := newInstallGateTestUpdater(t, component, &fakeDBCInstallGuard{events: &events}, nil, &events)
			install := u.installArtifact
			u.installArtifact = func(path string, cb mender.InstallProgressCallback) error {
				a, err := dbcstate.LoadActivationAttempt(u.activationAttempt)
				if err != nil || a.Component != component || a.CommittedArtifact != "release-v1.3.1" || a.Artifact != "release-v1.4.0" || a.BootID != "boot-current" || a.Phase != "installing" || a.OriginalSlot != "slot-a" || a.TargetSlot != "slot-b" {
					t.Fatalf("pre-install evidence %+v: %v", a, err)
				}
				return install(path, cb)
			}
			if err := u.installMender("image.mender"); err != nil {
				t.Fatal(err)
			}
			a, err := dbcstate.LoadActivationAttempt(u.activationAttempt)
			if err != nil || a.Phase != "staged" {
				t.Fatalf("post install %+v %v", a, err)
			}
		})
	}
}

func TestActivationInstallFailsClosedBeforeWrite(t *testing.T) {
	for _, kind := range []string{"marker-write", "quarantine", "quarantine-read", "no-identity", "bootenv"} {
		t.Run(kind, func(t *testing.T) {
			var events []string
			u := newInstallGateTestUpdater(t, "mdb", nil, nil, &events)
			switch kind {
			case "marker-write":
				u.activationAttempt = filepath.Join("/dev/null", "marker")
			case "quarantine":
				if err := u.gateStore.AddQuarantine("release-v1.4.0"); err != nil {
					t.Fatal(err)
				}
			case "quarantine-read":
				u.gateStore = commitgate.NewStoreAt("/dev/null/gate", "/dev/null/quarantine")
			case "no-identity":
				u.artifactInfo = func(string) (*mender.ArtifactInfo, error) { return &mender.ArtifactInfo{}, nil }
			case "bootenv":
				u.bootState = func() (mender.BootState, error) { return mender.BootState{}, errors.New("unknown bootenv") }
			}
			if err := u.installMender("image.mender"); err == nil {
				t.Fatal("install should fail")
			}
			if len(events) != 0 {
				t.Fatal("installer ran", events)
			}
		})
	}
}

func TestActivationRecoveryCrashGaps(t *testing.T) {
	for _, gap := range []string{"before-install", "after-staging", "after-commit", "after-rollback", "after-outcome"} {
		t.Run(gap, func(t *testing.T) {
			h := newActivationHarness(t, "mdb")
			phase := "installing"
			if gap == "after-rollback" || gap == "after-outcome" {
				phase = "rolling-back"
			}
			a := h.attempt(t, phase)
			switch gap {
			case "before-install":
				h.observation = mender.UpdateObservation{State: mender.StateNoUpdate, CommittedArtifact: h.origin.CommittedArtifact, CommittedVersion: "v1.3.1"}
				h.id = "boot-install"
			case "after-staging":
				h.trialBoot()
			case "after-commit":
				h.trialBoot()
				h.boot.UpgradeAvailable = false
				h.observation = mender.UpdateObservation{State: mender.StateNoUpdate, CommittedArtifact: h.origin.Artifact, CommittedVersion: "v1.4.0"}
			case "after-rollback", "after-outcome":
				h.observation = mender.UpdateObservation{State: mender.StateNoUpdate, CommittedArtifact: h.origin.CommittedArtifact, CommittedVersion: "v1.3.1"}
			}
			if gap == "after-outcome" {
				a.Phase = "rolled-back"
				a.ObservedBootID = h.id
				if err := dbcstate.SaveActivationAttempt(h.u.activationAttempt+".last", a); err != nil {
					t.Fatal(err)
				}
			}
			// Redis is deliberately absent from the classification evidence.
			if err := h.u.redis.GetClient().Raw().FlushAll(h.u.ctx).Err(); err != nil {
				t.Fatal(err)
			}
			if _, err := h.u.ReconcilePendingUpdate(); err != nil {
				t.Fatal(err)
			}
			if _, err := dbcstate.LoadActivationAttempt(h.u.activationAttempt); !os.IsNotExist(err) {
				t.Fatal("attempt not closed", err)
			}
			if h.rollbacks != 0 {
				t.Fatal("repeated rollback after settled state")
			}
			if gap != "after-staging" && h.commits != 0 {
				t.Fatal("repeated commit after settled state")
			}
		})
	}
}

func TestActivationSameVersionReinstallUsesSlot(t *testing.T) {
	for _, gated := range []bool{false, true} {
		t.Run(map[bool]string{false: "immediate", true: "gated"}[gated], func(t *testing.T) {
			h := newActivationHarness(t, "mdb")
			h.origin.Artifact = h.origin.CommittedArtifact
			h.observation.PendingArtifact = h.observation.CommittedArtifact
			h.observation.PendingVersion = h.observation.CommittedVersion
			h.attempt(t, "staged")
			h.pendingReboot()
			h.u.config.SetCommitGate(gated)
			rec, err := h.u.ReconcilePendingUpdate()
			if err != nil {
				t.Fatal(err)
			}
			if !rec.NeedsReboot || rec.Gated != nil || h.commits != 0 || h.rollbacks != 0 {
				t.Fatalf("same-version old slot mistaken for trial: %+v", rec)
			}
		})
	}
}

func TestActivationQuarantineAllowsNewerCandidate(t *testing.T) {
	h := newActivationHarness(t, "mdb")
	h.attempt(t, "staged")
	if _, err := h.u.ReconcilePendingUpdate(); err != nil {
		t.Fatal(err)
	}
	if err := h.u.recordInstallIntent("failed.mender"); err == nil || !strings.Contains(err.Error(), "quarantined") {
		t.Fatal("failed cached artifact was not refused", err)
	}
	h.u.artifactInfo = func(string) (*mender.ArtifactInfo, error) {
		return &mender.ArtifactInfo{ArtifactName: "release-v1.5.0"}, nil
	}
	if err := h.u.recordInstallIntent("newer.mender"); err != nil {
		t.Fatal("newer artifact blocked", err)
	}
	releases := h.u.withoutGateRejectedReleases([]Release{{TagName: "v1.4.0"}, {TagName: "v1.5.0"}})
	if len(releases) != 1 || releases[0].TagName != "v1.5.0" {
		t.Fatal("wrong release quarantine", releases)
	}
}
