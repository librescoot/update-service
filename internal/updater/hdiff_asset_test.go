package updater

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHDiffStagingAndChain(t *testing.T) {
	d := t.TempDir()
	name := "librescoot-unu-mdb-nightly-20261105T000000.hdelta"
	path := filepath.Join(d, name)
	if err := os.WriteFile(path, []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	_, deltas, err := listStagedArtifacts(d)
	if err != nil || len(deltas) != 1 || deltas[0] != path {
		t.Fatalf("staged deltas: %v, %v", deltas, err)
	}
	plan, err := planStagedArtifacts("nightly-20261104t000000", nil, deltas)
	if err != nil || len(plan.deltas) != 1 || plan.deltas[0] != path {
		t.Fatalf("plan: %+v, %v", plan, err)
	}
	if !isDeltaPath(path) || isDeltaPath(path+".tmp") {
		t.Fatal("incorrect delta suffix detection")
	}
}

func TestReleaseAssetHDiffCompatibility(t *testing.T) {
	d := t.TempDir()
	t.Setenv("PATH", d)
	tag := "nightly-20261105T000000"
	normal := Asset{Name: "librescoot-unu-mdb-" + tag + ".delta", Size: 100}
	hd := Asset{Name: "librescoot-unu-mdb-" + tag + ".hdelta", Size: 50}
	r := Release{TagName: tag, Assets: []Asset{hd, normal}}
	if a, ok := releaseAsset(r, "unu-mdb", ".delta"); !ok || a.Name != normal.Name {
		t.Fatal("must use xdelta when hpatchz is unavailable", a)
	}
	if err := os.WriteFile(filepath.Join(d, "hpatchz"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if a, ok := releaseAsset(r, "unu-mdb", ".delta"); !ok || a.Name != hd.Name {
		t.Fatal("must choose smaller hdiff", a)
	}
	r.Assets[0].Size = 101
	if a, ok := releaseAsset(r, "unu-mdb", ".delta"); !ok || a.Name != normal.Name {
		t.Fatal("must retain smaller xdelta", a)
	}
	if _, ok := releaseAsset(r, "unu-dbc", ".delta"); ok {
		t.Fatal("wrong board selected")
	}
	r.TagName = "testing-20261105T000000"
	r.Assets = []Asset{{Name: "librescoot-unu-mdb-" + r.TagName + ".delta", Size: 100}, {Name: "librescoot-unu-mdb-" + r.TagName + ".hdelta", Size: 1}}
	if a, ok := releaseAsset(r, "unu-mdb", ".delta"); !ok || a.Name != r.Assets[0].Name {
		t.Fatal("testing must use the standard delta", a)
	}
}
