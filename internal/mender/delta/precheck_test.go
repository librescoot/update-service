package delta

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type tarEntry struct {
	name string
	body []byte
}

func writeTar(t *testing.T, path string, gzipped bool, entries []tarEntry) string {
	t.Helper()

	var buf bytes.Buffer
	var w io.Writer = &buf
	var gz *gzip.Writer
	if gzipped {
		gz = gzip.NewWriter(&buf)
		w = gz
	}

	tw := tar.NewWriter(w)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0644, Size: int64(len(e.body)), Typeflag: tar.TypeReg}
		if strings.HasSuffix(e.name, "/") {
			hdr.Typeflag, hdr.Size = tar.TypeDir, 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %s: %v", e.name, err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write(e.body); err != nil {
				t.Fatalf("write body %s: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if gz != nil {
		if err := gz.Close(); err != nil {
			t.Fatalf("close gzip: %v", err)
		}
	}
	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// makeDelta writes a delta laid out the way mender-delta-create.py writes one:
// metadata.json first, then the new_files and patches trees.
func makeDelta(t *testing.T, path, oldSum, newSum string) string {
	t.Helper()
	meta := DeltaMetadata{
		OldPayloadChecksum: oldSum,
		NewPayloadChecksum: newSum,
		Version:            3,
		Changes: map[string]ChangeInfo{
			"data/0000.tar.gz": {
				Type:    "modified",
				Patch:   "data_0000.tar.gz.xdelta",
				OldMeta: FileMeta{Compressed: true, DecompressedSize: 4096},
			},
		},
	}
	body, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	return writeTar(t, path, true, []tarEntry{
		{name: "./metadata.json", body: body},
		{name: "./new_files/", body: nil},
		{name: "./new_files/manifest", body: []byte("x")},
		{name: "./patches/", body: nil},
		{name: "./patches/data_0000.tar.gz.xdelta", body: []byte("patch")},
	})
}

// makeMender writes an artifact with v3 member ordering and a payload large
// enough that reading it instead of seeking past it would be obvious.
func makeMender(t *testing.T, path, rootfsSum string) string {
	t.Helper()
	manifest := rootfsSum + "  data/0000/rootfs.ext4\n" +
		strings.Repeat("a", 64) + "  header.tar.gz\n" +
		strings.Repeat("b", 64) + "  version\n"
	return writeTar(t, path, false, []tarEntry{
		{name: "version", body: []byte(`{"format":"mender","version":3}`)},
		{name: "manifest", body: []byte(manifest)},
		{name: "header.tar.gz", body: []byte("header")},
		{name: "data/0000.tar.gz", body: bytes.Repeat([]byte("p"), 1<<20)},
	})
}

func testApplier() *Applier {
	return NewApplier(log.New(io.Discard, "", 0), os.TempDir())
}

const (
	sumA = "aaaa000000000000000000000000000000000000000000000000000000000001"
	sumB = "bbbb000000000000000000000000000000000000000000000000000000000002"
	sumC = "cccc000000000000000000000000000000000000000000000000000000000003"
)

func TestReadMetadata(t *testing.T) {
	p := makeDelta(t, filepath.Join(t.TempDir(), "a.delta"), sumA, sumB)

	meta, err := ReadMetadata(p)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if meta.OldPayloadChecksum != sumA || meta.NewPayloadChecksum != sumB {
		t.Errorf("got old=%s new=%s", meta.OldPayloadChecksum, meta.NewPayloadChecksum)
	}
	if meta.Version != 3 {
		t.Errorf("version = %d, want 3", meta.Version)
	}
}

// ShellTarExtract accepts an uncompressed tar, so ReadMetadata must too.
func TestReadMetadataPlainTar(t *testing.T) {
	body, err := json.Marshal(DeltaMetadata{OldPayloadChecksum: sumA, NewPayloadChecksum: sumB, Version: 3})
	if err != nil {
		t.Fatal(err)
	}
	p := writeTar(t, filepath.Join(t.TempDir(), "plain.delta"), false, []tarEntry{
		{name: "./metadata.json", body: body},
	})

	meta, err := ReadMetadata(p)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if meta.OldPayloadChecksum != sumA {
		t.Errorf("old = %s, want %s", meta.OldPayloadChecksum, sumA)
	}
}

// Deltas built before the ordering was pinned put metadata.json after the "./"
// directory entry, so the member is found by name rather than by position.
func TestReadMetadataUnorderedArchive(t *testing.T) {
	body, err := json.Marshal(DeltaMetadata{OldPayloadChecksum: sumA, Version: 3})
	if err != nil {
		t.Fatal(err)
	}
	p := writeTar(t, filepath.Join(t.TempDir(), "old.delta"), true, []tarEntry{
		{name: "./", body: nil},
		{name: "./patches/data_0000.tar.gz.xdelta", body: []byte("patch")},
		{name: "./metadata.json", body: body},
	})

	meta, err := ReadMetadata(p)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if meta.OldPayloadChecksum != sumA {
		t.Errorf("old = %s, want %s", meta.OldPayloadChecksum, sumA)
	}
}

func TestReadMetadataMissing(t *testing.T) {
	p := writeTar(t, filepath.Join(t.TempDir(), "empty.delta"), true, []tarEntry{
		{name: "./patches/x.xdelta", body: []byte("patch")},
	})

	if _, err := ReadMetadata(p); err == nil {
		t.Fatal("expected an error for a delta with no metadata.json")
	}
}

func TestBaseRootfsChecksum(t *testing.T) {
	p := makeMender(t, filepath.Join(t.TempDir(), "base.mender"), sumA)

	got, err := BaseRootfsChecksum(p)
	if err != nil {
		t.Fatalf("BaseRootfsChecksum: %v", err)
	}
	if got != sumA {
		t.Errorf("got %s, want %s", got, sumA)
	}
}

func TestBaseRootfsChecksumNoManifest(t *testing.T) {
	p := writeTar(t, filepath.Join(t.TempDir(), "bad.mender"), false, []tarEntry{
		{name: "version", body: []byte("{}")},
	})

	if _, err := BaseRootfsChecksum(p); err == nil {
		t.Fatal("expected an error for an artifact with no manifest")
	}
}

func TestVerifyChainApplies(t *testing.T) {
	tests := []struct {
		name      string
		baseSum   string
		links     [][2]string // old, new per delta
		wantMatch bool        // expect ErrBaseMismatch
	}{
		{name: "single delta on its base", baseSum: sumA, links: [][2]string{{sumA, sumB}}},
		{name: "single delta on the wrong base", baseSum: sumB, links: [][2]string{{sumA, sumB}}, wantMatch: true},
		{name: "linked chain", baseSum: sumA, links: [][2]string{{sumA, sumB}, {sumB, sumC}}},
		{name: "chain on the wrong base", baseSum: sumC, links: [][2]string{{sumA, sumB}, {sumB, sumC}}, wantMatch: true},
		{name: "chain with a gap", baseSum: sumA, links: [][2]string{{sumA, sumB}, {sumC, sumA}}, wantMatch: true},
		{name: "chain in the wrong order", baseSum: sumB, links: [][2]string{{sumB, sumC}, {sumA, sumB}}, wantMatch: true},
		{name: "delta without checksums is skipped", baseSum: sumA, links: [][2]string{{"", ""}}},
		{name: "unverifiable link does not poison the next", baseSum: sumA, links: [][2]string{{sumA, ""}, {sumB, sumC}}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			base := makeMender(t, filepath.Join(dir, "base.mender"), tc.baseSum)

			paths := make([]string, len(tc.links))
			for i, l := range tc.links {
				paths[i] = makeDelta(t, filepath.Join(dir, string(rune('a'+i))+".delta"), l[0], l[1])
			}

			err := testApplier().verifyChainApplies(base, paths)
			if tc.wantMatch {
				if !errors.Is(err, ErrBaseMismatch) {
					t.Fatalf("got %v, want ErrBaseMismatch", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// An unreadable base is the unpack's error to report; the precheck must not
// convert it into a mismatch and mask the real cause.
func TestVerifyChainAppliesUnreadableBase(t *testing.T) {
	dir := t.TempDir()
	d := makeDelta(t, filepath.Join(dir, "a.delta"), sumA, sumB)

	if err := testApplier().verifyChainApplies(filepath.Join(dir, "missing.mender"), []string{d}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestVerifyChainAppliesUnreadableDelta(t *testing.T) {
	dir := t.TempDir()
	base := makeMender(t, filepath.Join(dir, "base.mender"), sumA)

	err := testApplier().verifyChainApplies(base, []string{filepath.Join(dir, "missing.delta")})
	if err == nil {
		t.Fatal("expected an error for a delta that cannot be read")
	}
	if errors.Is(err, ErrBaseMismatch) {
		t.Fatalf("an unreadable delta is not a base mismatch: %v", err)
	}
}

// ApplyChain must reject a mismatched chain before it unpacks anything, so a
// bogus temp dir it would otherwise need is never reached.
func TestApplyChainRejectsBeforeUnpacking(t *testing.T) {
	dir := t.TempDir()
	base := makeMender(t, filepath.Join(dir, "base.mender"), sumB)
	d := makeDelta(t, filepath.Join(dir, "a.delta"), sumA, sumB)

	a := NewApplier(log.New(io.Discard, "", 0), filepath.Join(dir, "nonexistent-temp"))
	err := a.ApplyChain(context.Background(), base, []string{d}, filepath.Join(dir, "out.mender"), func(int, string) {})
	if !errors.Is(err, ErrBaseMismatch) {
		t.Fatalf("got %v, want ErrBaseMismatch", err)
	}
}

// makeNamedDelta is makeDelta with the artifact-name fields set, which is what
// lets the mismatch error name versions instead of digests.
func makeNamedDelta(t *testing.T, path, oldSum, newSum, oldName, newName string) string {
	t.Helper()
	meta := DeltaMetadata{
		OldArtifactName:    oldName,
		NewArtifactName:    newName,
		OldPayloadChecksum: oldSum,
		NewPayloadChecksum: newSum,
		Version:            3,
		Changes: map[string]ChangeInfo{
			"data/0000.tar.gz": {Type: "modified", Patch: "data_0000.tar.gz.xdelta"},
		},
	}
	body, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	return writeTar(t, path, true, []tarEntry{{name: "./metadata.json", body: body}})
}

// The 2026-08-24 chain hole, as the BLE path hits it: a phone pushes the delta
// published on the 133115 release to a scooter running 082958. The delta was
// built against 123219, so it cannot apply, and the message has to say which
// delta would, because it is all the phone shows.
func TestVerifyChainAppliesNamesTheVersions(t *testing.T) {
	dir := t.TempDir()
	base := makeMender(t, filepath.Join(dir, "librescoot-unu-mdb-nightly-20260823T082958.mender"), sumA)
	d := makeNamedDelta(t,
		filepath.Join(dir, "librescoot-unu-mdb-nightly-20260824T133115.delta"),
		sumB, sumC,
		"release-nightly-20260824T123219",
		"release-nightly-20260824T133115")

	err := testApplier().verifyChainApplies(base, []string{d})
	if !errors.Is(err, ErrBaseMismatch) {
		t.Fatalf("got %v, want ErrBaseMismatch", err)
	}
	for _, want := range []string{
		"delta 1/1 (nightly-20260824T133115)",
		"needs base nightly-20260824T123219",
		"have nightly-20260823T082958",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err.Error(), want)
		}
	}
}

// Deltas predating the artifact-name fields still have to say something useful,
// so the digest remains the fallback rather than an empty label.
func TestVerifyChainAppliesFallsBackToDigest(t *testing.T) {
	dir := t.TempDir()
	base := makeMender(t, filepath.Join(dir, "base.mender"), sumA)
	d := makeDelta(t, filepath.Join(dir, "unnamed.delta"), sumB, sumC)

	err := testApplier().verifyChainApplies(base, []string{d})
	if !errors.Is(err, ErrBaseMismatch) {
		t.Fatalf("got %v, want ErrBaseMismatch", err)
	}
	if !strings.Contains(err.Error(), "rootfs "+sumB[:12]) {
		t.Errorf("error %q does not name the wanted base digest", err.Error())
	}
	if !strings.Contains(err.Error(), "rootfs "+sumA[:12]) {
		t.Errorf("error %q does not name the base digest we have", err.Error())
	}
}
