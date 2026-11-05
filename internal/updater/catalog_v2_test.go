package updater

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/librescoot/update-service/internal/catalog"
	"github.com/librescoot/update-service/internal/mender"
	"github.com/librescoot/update-service/internal/mender/delta"
)

func planArtifact(version string) catalog.Artifact {
	a := catalog.Artifact{Variant: "unu-mdb", Version: version, Name: "release-" + version, Devices: []string{"unu-mdb"}, ManifestSHA256: catalog.Hash([]byte("m" + version)), HeaderSHA256: catalog.Hash([]byte("h" + version)), RootfsSHA256: catalog.Hash([]byte("r" + version)), PayloadSHA256: catalog.Hash([]byte("p" + version)), PayloadSize: 2048, Full: catalog.Object{URL: "https://example.org/librescoot-unu-mdb-" + version + ".mender", Size: 1000, SHA256: catalog.Hash([]byte("f" + version))}}
	a.ID = a.Identity()
	return a
}
func TestV2DeltaMetadataBindsBothEndpoints(t *testing.T) {
	a, b := planArtifact("v1.4.1"), planArtifact("testing-20261105T120000")
	p := catalog.Plan{Nodes: []catalog.Artifact{a, b}, Steps: []catalog.Edge{{Source: a.ID, Target: b.ID}}}
	meta := delta.DeltaMetadata{OldArtifactName: a.Name, NewArtifactName: b.Name, OldPayloadChecksum: a.RootfsSHA256, NewPayloadChecksum: b.RootfsSHA256,
		Changes: map[string]delta.ChangeInfo{"data/0000.tar.gz": {Type: "modified", OldMeta: delta.FileMeta{DecompressedSHA256: a.PayloadSHA256}, NewMeta: delta.FileMeta{DecompressedSHA256: b.PayloadSHA256}}}}
	write := func() string {
		file := filepath.Join(t.TempDir(), "edge.delta")
		f, err := os.Create(file)
		if err != nil {
			t.Fatal(err)
		}
		gz := gzip.NewWriter(f)
		tw := tar.NewWriter(gz)
		data, _ := json.Marshal(meta)
		if err := tw.WriteHeader(&tar.Header{Name: "metadata.json", Mode: 0600, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		return file
	}
	if err := verifyV2Deltas(p, []string{write()}); err != nil {
		t.Fatal(err)
	}
	meta.OldPayloadChecksum = b.RootfsSHA256
	if err := verifyV2Deltas(p, []string{write()}); err == nil {
		t.Fatal("wrong base accepted")
	}
}

func TestFinishPlanOnlyAfterMatchingCommit(t *testing.T) {
	u, _ := newTestUpdaterForPreview(t, nil)
	dir := t.TempDir()
	u.mender = mender.NewManager(dir, nil, u.logger)
	p := catalog.Plan{Schema: 2, Revision: catalog.Hash([]byte("rev")), Channel: "stable", Target: planArtifact("v1.4.2"), InstallStarted: true}
	if err := saveV2Plan(dir, p); err != nil {
		t.Fatal(err)
	}
	u.finishV2Plan("release-v1.4.1")
	if _, err := loadV2Plan(dir); err != nil {
		t.Fatal("mismatched commit removed plan", err)
	}
	u.finishV2Plan("release-v1.4.2")
	if _, err := os.Stat(planFile(dir)); !os.IsNotExist(err) {
		t.Fatal("matching committed plan retained")
	}
}

func TestRecoveryDoesNotRetryUnconfirmedInstall(t *testing.T) {
	u, mr := newTestUpdaterForPreview(t, nil)
	dir := t.TempDir()
	u.mender = mender.NewManager(dir, nil, u.logger)
	p := catalog.Plan{Schema: 2, Revision: catalog.Hash([]byte("rev")), Channel: "stable", Target: planArtifact("v1.4.2"), InstallStarted: true}
	if err := saveV2Plan(dir, p); err != nil {
		t.Fatal(err)
	}
	u.runningVersion = func() (string, error) { return "v1.4.1", nil }
	if !u.recoverV2Plan(false) {
		t.Fatal("unconfirmed plan not retained")
	}
	if got := mr.HGet("ota", "error:mdb"); got != "v2-install-unconfirmed" {
		t.Fatalf("wrong status %q", got)
	}
	if _, err := loadV2Plan(dir); err != nil {
		t.Fatal("lost recovery plan", err)
	}
	u.runningVersion = func() (string, error) { return "v1.4.2", nil }
	if !u.recoverV2Plan(true) {
		t.Fatal("pending target not retained")
	}
	if u.recoverV2Plan(false) {
		t.Fatal("committed target left plan active")
	}
	if _, err := os.Stat(planFile(dir)); !os.IsNotExist(err) {
		t.Fatal("committed plan not cleared")
	}
}

func TestCancelPlanRefusesPendingMender(t *testing.T) {
	u, _ := newTestUpdaterForPreview(t, nil)
	dir := t.TempDir()
	u.mender = mender.NewManager(dir, nil, u.logger)
	p := catalog.Plan{Schema: 2, Revision: catalog.Hash([]byte("rev")), Channel: "stable", Target: planArtifact("v1.4.1")}
	if err := saveV2Plan(dir, p); err != nil {
		t.Fatal(err)
	}
	u.observeUpdate = func() (mender.UpdateObservation, error) {
		return mender.UpdateObservation{State: mender.StateNeedsReboot, PendingArtifact: "release-v1.4.1"}, nil
	}
	u.cancelV2Plan()
	if _, err := loadV2Plan(dir); err != nil {
		t.Fatal("pending plan removed", err)
	}
	u.observeUpdate = func() (mender.UpdateObservation, error) {
		return mender.UpdateObservation{State: mender.StateNoUpdate}, nil
	}
	u.cancelV2Plan()
	if _, err := os.Stat(planFile(dir)); !os.IsNotExist(err) {
		t.Fatal("plan not canceled")
	}
}

func TestDurableV2Plan(t *testing.T) {
	a, b := planArtifact("v1.4.1"), planArtifact("testing-20261105T120000")
	edge := catalog.Edge{Source: a.ID, Target: b.ID, URL: "https://example.org/librescoot-unu-mdb-from-" + a.ID + "-to-" + b.ID + ".delta", Size: 10, SHA256: catalog.Hash([]byte("delta"))}
	c := catalog.Catalog{Schema: 2, Revision: catalog.Hash([]byte("catalog")), Artifacts: []catalog.Artifact{a, b}, Deltas: []catalog.Edge{edge}}
	p := c.Route(a, b, "testing", true)
	dir := t.TempDir()
	if err := saveV2Plan(dir, p); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadV2Plan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Target.ID != b.ID || loaded.Base.ID != a.ID {
		t.Fatal("plan target/base changed")
	}
	if err := os.WriteFile(filepath.Join(dir, "plan-v2.json"), []byte(`{"schema_version":2}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadV2Plan(dir); err == nil {
		t.Fatal("invalid saved plan accepted")
	}
	if err := clearV2Plan(dir); err != nil {
		t.Fatal(err)
	}
}
