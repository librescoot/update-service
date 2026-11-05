package updater

import (
	"os"
	"strings"
	"testing"

	"github.com/librescoot/update-service/internal/mender"
)

func TestSwitchSurvivesMDBRestartUntilBothBoardsCommit(t *testing.T) {
	u, mr := newTestUpdaterForPreview(t, nil)
	dir := t.TempDir()
	u.mender = mender.NewManager(dir, nil, u.logger)
	u.observeUpdate = func() (mender.UpdateObservation, error) {
		return mender.UpdateObservation{State: mender.StateNoUpdate}, nil
	}
	mr.HSet("version:mdb", "version_id", "v1.4.1")
	mr.HSet("version:dbc", "version_id", "v1.4.1")
	mr.HSet("ota", "status:mdb", "idle")
	mr.HSet("ota", "status:dbc", "idle")
	id := strings.Repeat("a", 32)
	u.requestSwitch("testing", id)
	if got := mr.HGet("ota", "switch-status"); got != "accepted" {
		t.Fatalf("request not accepted: %q", got)
	}
	u.advanceSwitch()
	if got := mr.HGet("settings", "updates.dbc.channel"); got != "testing" {
		t.Fatalf("DBC channel: %q", got)
	}
	// MDB committed, DBC is still off. A fresh Updater instance reloads the journal.
	mr.HSet("version:mdb", "version_id", "testing-20261105t120000")
	mr.HSet("ota", "committed-version:mdb", "testing-20261105t120000")
	u.advanceSwitch()
	s, err := readSwitch(dir)
	if err != nil || !s.MDBDone || s.DBCDone {
		t.Fatalf("wrong partial state: %+v, %v", s, err)
	}
	u.switchQueuedMDB = false
	u.switchQueuedDBC = false
	mr.HDel("settings", "updates.dbc.channel")
	u.advanceSwitch()
	if got := mr.HGet("settings", "updates.dbc.channel"); got != "testing" {
		t.Fatal("DBC intent lost after restart")
	}
	// Running new rootfs is not proof of a commit.
	mr.HSet("version:dbc", "version_id", "testing-20261105t120000")
	mr.HSet("ota", "status:dbc", "pending-reboot")
	u.advanceSwitch()
	s, _ = readSwitch(dir)
	if s.DBCDone {
		t.Fatal("pending DBC marked complete")
	}
	mr.HSet("ota", "status:dbc", "idle")
	mr.HSet("ota", "committed-version:dbc", "testing-20261105t120000")
	u.advanceSwitch()
	s, _ = readSwitch(dir)
	if !s.MDBDone || !s.DBCDone || mr.HGet("ota", "switch-status") != "complete" {
		t.Fatalf("not completed: %+v", s)
	}
}
func TestSwitchWakesCurrentDBCForCommitAttestation(t *testing.T) {
	u, mr := newTestUpdaterForPreview(t, stableIndex())
	dir := t.TempDir()
	u.mender = mender.NewManager(dir, nil, u.logger)
	mr.HSet("settings", "updates.dbc.channel", "stable")
	mr.HSet("version:dbc", "variant_id", "unu-dbc")
	mr.HSet("version:dbc", "version_id", "v1.3.0")
	if err := saveSwitch(dir, switchIntent{ID: strings.Repeat("a", 32), Channel: "stable"}); err != nil {
		t.Fatal(err)
	}
	got := u.preflightDBCUpdate(latestManifest(stableIndex()))
	if !got.wake {
		t.Fatalf("DBC not woken for commit attestation: %+v", got)
	}
}

func TestSwitchRefusesMenderPendingEvenIfStatusIsIdle(t *testing.T) {
	u, mr := newTestUpdaterForPreview(t, nil)
	u.mender = mender.NewManager(t.TempDir(), nil, u.logger)
	u.observeUpdate = func() (mender.UpdateObservation, error) {
		return mender.UpdateObservation{State: mender.StateNeedsReboot, PendingArtifact: "release-v1.4.2"}, nil
	}
	mr.HSet("ota", "status:mdb", "idle")
	mr.HSet("ota", "status:dbc", "idle")
	u.requestSwitch("testing", strings.Repeat("d", 32))
	if mr.HGet("ota", "switch-status") != "busy" {
		t.Fatal("pending Mender request accepted")
	}
	if _, err := readSwitch(u.mender.GetDownloadDir()); !os.IsNotExist(err) {
		t.Fatal("pending request persisted")
	}
}

func TestSwitchReportsPartialWhenBoardHasNoRelease(t *testing.T) {
	u, mr := newTestUpdaterForPreview(t, nil)
	u.mender = mender.NewManager(t.TempDir(), nil, u.logger)
	u.observeUpdate = func() (mender.UpdateObservation, error) {
		return mender.UpdateObservation{State: mender.StateNoUpdate}, nil
	}
	mr.HSet("ota", "status:mdb", "idle")
	mr.HSet("ota", "status:dbc", "idle")
	mr.HSet("settings", "updates.dbc.last-check-time", "old")
	u.requestSwitch("testing", strings.Repeat("c", 32))
	u.advanceSwitch()
	mr.HSet("version:mdb", "version_id", "testing-20261105t120000")
	mr.HSet("ota", "committed-version:mdb", "testing-20261105t120000")
	mr.HSet("settings", "updates.dbc.last-attempt-result", "no-release")
	mr.HSet("settings", "updates.dbc.last-check-time", "new")
	u.advanceSwitch()
	s, err := readSwitch(u.mender.GetDownloadDir())
	if err != nil || !s.MDBDone || !s.DBCDone || !s.DBCUnavailable || mr.HGet("ota", "switch-status") != "partial" {
		t.Fatalf("wrong partial result: %+v, %v", s, err)
	}
}

func TestSwitchRejectsConcurrentRequestAndPersistsFailure(t *testing.T) {
	u, mr := newTestUpdaterForPreview(t, nil)
	u.mender = mender.NewManager(t.TempDir(), nil, u.logger)
	u.observeUpdate = func() (mender.UpdateObservation, error) {
		return mender.UpdateObservation{State: mender.StateNoUpdate}, nil
	}
	mr.HSet("ota", "status:mdb", "idle")
	mr.HSet("ota", "status:dbc", "idle")
	first, second := strings.Repeat("a", 32), strings.Repeat("b", 32)
	u.requestSwitch("testing", first)
	u.requestSwitch("stable", second)
	if mr.HGet("ota", "switch-status") != "busy" {
		t.Fatal("overlapping request accepted")
	}
	s, err := readSwitch(u.mender.GetDownloadDir())
	if err != nil || s.ID != first {
		t.Fatal("first request lost")
	}
	mr.HSet("ota", "status:dbc", "error")
	u.advanceSwitch()
	s, _ = readSwitch(u.mender.GetDownloadDir())
	if !s.Failed {
		t.Fatal("error not persisted")
	}
}
