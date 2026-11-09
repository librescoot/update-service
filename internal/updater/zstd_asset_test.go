package updater

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReleaseAssetZstdImage(t *testing.T) {
	d := t.TempDir()
	t.Setenv("PATH", d)
	tag := "nightly-20261109T030336"
	gz := Asset{Name: "librescoot-unu-mdb-" + tag + ".mender", Size: 182}
	zst := Asset{Name: "librescoot-unu-mdb-" + tag + ".zmender", Size: 133}
	r := Release{TagName: tag, Assets: []Asset{zst, gz}}

	if a, ok := releaseAsset(r, "unu-mdb", ".mender"); !ok || a.Name != gz.Name {
		t.Fatal("without the zstd tool the gzip artifact is the only choice", a)
	}
	if err := os.WriteFile(filepath.Join(d, "zstd"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if a, ok := releaseAsset(r, "unu-mdb", ".mender"); !ok || a.Name != zst.Name {
		t.Fatal("with the tool the smaller zstd artifact must win", a)
	}

	r.Assets[0].Size = 190
	if a, ok := releaseAsset(r, "unu-mdb", ".mender"); !ok || a.Name != gz.Name {
		t.Fatal("a zstd artifact that is not smaller must not be used", a)
	}
	r.Assets[0].Size = 0
	if a, ok := releaseAsset(r, "unu-mdb", ".mender"); !ok || a.Name != gz.Name {
		t.Fatal("an empty zstd artifact must not be used", a)
	}

	// Deltas are unaffected by the artifact's payload compression.
	delta := Asset{Name: "librescoot-unu-mdb-" + tag + ".delta", Size: 10}
	r.Assets = append(r.Assets, delta, Asset{Name: zst.Name, Size: 1})
	if a, ok := releaseAsset(r, "unu-mdb", ".delta"); !ok || a.Name != delta.Name {
		t.Fatal("zstd must not affect delta selection", a)
	}

	// A release that only carries the zstd artifact has no .mender for a
	// system that cannot read it.
	t.Setenv("PATH", t.TempDir())
	only := Release{TagName: tag, Assets: []Asset{{Name: zst.Name, Size: 133}}}
	if _, ok := releaseAsset(only, "unu-mdb", ".mender"); ok {
		t.Fatal("zstd-only release must not be offered without the tool")
	}
	if _, ok := releaseAsset(only, "unu-dbc", ".mender"); ok {
		t.Fatal("wrong board selected")
	}
}
