package updater

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReleaseAssetHDiffCompatibility(t *testing.T) {
	d := t.TempDir()
	t.Setenv("PATH", d)
	tag := "nightly-20261105T000000"
	normal := Asset{Name: "librescoot-unu-mdb-" + tag + ".delta", Size: 100}
	hd := Asset{Name: "librescoot-unu-mdb-" + tag + ".hdiff.delta", Size: 50}
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
}
