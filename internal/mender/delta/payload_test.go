package delta

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func requireTools(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		if _, err := exec.LookPath(n); err != nil {
			t.Skipf("%s not installed", n)
		}
	}
}

func hexSum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func zstdBytes(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gzipBytes(t *testing.T, raw []byte) []byte {
	t.Helper()
	cmd := exec.Command("gzip", "-c")
	cmd.Stdin = bytes.NewReader(raw)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// payloadTarBytes returns a payload tar holding one rootfs file.
func payloadTarBytes(t *testing.T, rootfs []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "rootfs.ext4", Mode: 0644, Size: int64(len(rootfs)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(rootfs); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func menderWith(t *testing.T, path, member string, compressed []byte, rootfsSum string) string {
	t.Helper()
	manifest := rootfsSum + "  data/0000/rootfs.ext4\n" + strings.Repeat("a", 64) + "  header.tar.gz\n"
	return writeTar(t, path, false, []tarEntry{
		{name: "version", body: []byte(`{"format":"mender","version":3}`)},
		{name: "manifest", body: []byte(manifest)},
		{name: "header.tar.gz", body: []byte("header")},
		{name: member, body: compressed},
	})
}

func TestBasePayloadFormat(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		member string
		want   PayloadFormat
	}{
		{"data/0000.tar.gz", PayloadGzip},
		{"data/0000.tar.zst", PayloadZstd},
	} {
		p := menderWith(t, filepath.Join(dir, strings.ReplaceAll(tc.member, "/", "_")+".mender"), tc.member, bytes.Repeat([]byte("x"), 2048), sumA)
		got, err := BasePayloadFormat(p)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %q, %v, want %q", tc.member, got, err, tc.want)
		}
	}

	odd := menderWith(t, filepath.Join(dir, "odd.mender"), "data/0000.tar.xz", []byte("x"), sumA)
	if _, err := BasePayloadFormat(odd); err == nil {
		t.Error("an xz payload must be reported, not guessed at")
	}
}

func TestZstdCompressAndDecompressRoundTrip(t *testing.T) {
	requireTools(t, "zstd")
	dir := t.TempDir()
	rootfs := bytes.Repeat([]byte("rootfs-content-"), 20000)
	src := filepath.Join(dir, "payload.tar")
	if err := os.WriteFile(src, payloadTarBytes(t, rootfs), 0600); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "payload.tar.zst")
	got, err := compressPayloadAndHash(PayloadZstd, src, out, newProgressTracker(0, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got != hexSum(rootfs) {
		t.Errorf("checksum = %s, want %s", got, hexSum(rootfs))
	}

	// The Go decoder is what ReadArtifactInfo uses, so it must accept what the
	// tool wrote.
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := zstd.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	back, err := io.ReadAll(dec)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(src)
	if !bytes.Equal(back, want) {
		t.Fatal("zstd output does not decompress to the input")
	}

	plain := filepath.Join(dir, "back.tar")
	if err := decompressPayload(PayloadZstd, out, plain, newProgressTracker(0, nil)); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(plain); !bytes.Equal(b, want) {
		t.Fatal("decompressPayload differs from the input")
	}
}

func TestZstdWithoutToolIsRefused(t *testing.T) {
	orig := lookPath
	lookPath = func(name string) (string, error) {
		if name == "zstd" {
			return "", exec.ErrNotFound
		}
		return orig(name)
	}
	defer func() { lookPath = orig }()

	dir := t.TempDir()
	if ZstdAvailable() {
		t.Fatal("ZstdAvailable must follow lookPath")
	}
	if err := decompressPayload(PayloadZstd, "in", filepath.Join(dir, "out"), nil); !errors.Is(err, ErrZstdUnavailable) {
		t.Errorf("decompress: %v", err)
	}
	if _, err := compressPayloadAndHash(PayloadZstd, filepath.Join(dir, "in"), filepath.Join(dir, "out"), nil); !errors.Is(err, ErrZstdUnavailable) {
		t.Errorf("compress: %v", err)
	}

	// A zstd base is turned away before anything is unpacked.
	base := menderWith(t, filepath.Join(dir, "base.mender"), "data/0000.tar.zst", []byte("x"), sumA)
	delta := makeDelta(t, filepath.Join(dir, "a.delta"), sumA, sumB)
	err := testApplier().ApplyChain(context.Background(), base, []string{delta}, filepath.Join(dir, "out.mender"), func(int, string) {})
	if !errors.Is(err, ErrZstdUnavailable) {
		t.Errorf("ApplyChain: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.mender")); err == nil {
		t.Error("no output may be written")
	}
}

// buildChainFixture writes a base artifact in the requested payload format and a
// delta from it to a changed rootfs. The change name is the target artifact's
// member, which can differ from the base compression.
func buildChainFixture(t *testing.T, dir string, format PayloadFormat, changeName string) (base, delta string, newRootfs []byte, newTar []byte) {
	t.Helper()
	oldRootfs := bytes.Repeat([]byte("old rootfs block "), 30000)
	newRootfs = append(bytes.Clone(oldRootfs), []byte("and something new")...)
	oldTar, newTar := payloadTarBytes(t, oldRootfs), payloadTarBytes(t, newRootfs)

	var member string
	var compressed []byte
	if format == PayloadZstd {
		member, compressed = "data/0000.tar.zst", zstdBytes(t, oldTar)
	} else {
		member, compressed = "data/0000.tar.gz", gzipBytes(t, oldTar)
	}
	base = menderWith(t, filepath.Join(dir, "base.mender"), member, compressed, hexSum(oldRootfs))

	oldPath, newPath, patch := filepath.Join(dir, "old.tar"), filepath.Join(dir, "new.tar"), filepath.Join(dir, "p.xdelta")
	if err := os.WriteFile(oldPath, oldTar, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, newTar, 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("xdelta3", "-e", "-S", "none", "-s", oldPath, newPath, patch).CombinedOutput(); err != nil {
		t.Fatalf("xdelta3: %v\n%s", err, out)
	}
	patchBytes, err := os.ReadFile(patch)
	if err != nil {
		t.Fatal(err)
	}

	newManifest := hexSum(newRootfs) + "  data/0000/rootfs.ext4\n" + strings.Repeat("a", 64) + "  header.tar.gz\n"
	meta := DeltaMetadata{
		OldPayloadChecksum: hexSum(oldRootfs),
		NewPayloadChecksum: hexSum(newRootfs),
		Version:            3,
		Changes: map[string]ChangeInfo{
			changeName: {
				Type:    "modified",
				Patch:   "data_0000.tar.gz.xdelta",
				OldMeta: FileMeta{Compressed: true, DecompressedSize: int64(len(oldTar))},
				NewMeta: FileMeta{Compressed: true, DecompressedSHA256: hexSum(newTar)},
			},
			"manifest":      {Type: "new"},
			"version":       {Type: "new"},
			"header.tar.gz": {Type: "new"},
		},
	}
	body, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	delta = writeTar(t, filepath.Join(dir, "d.delta"), true, []tarEntry{
		{name: "./metadata.json", body: body},
		{name: "./new_files/manifest", body: []byte(newManifest)},
		{name: "./new_files/version", body: []byte(`{"format":"mender","version":3}`)},
		{name: "./new_files/header.tar.gz", body: []byte("header")},
		{name: "./patches/data_0000.tar.gz.xdelta", body: patchBytes},
	})
	return base, delta, newRootfs, newTar
}

func TestApplyChainKeepsBasePayloadFormat(t *testing.T) {
	requireTools(t, "xdelta3", "gzip", "tar")
	for _, tc := range []struct {
		format     PayloadFormat
		changeName string
	}{
		{PayloadGzip, "data/0000.tar.gz"}, {PayloadGzip, "data/0000.tar.zst"},
		{PayloadZstd, "data/0000.tar.gz"}, {PayloadZstd, "data/0000.tar.zst"},
	} {
		format := tc.format
		t.Run(string(format)+"/"+filepath.Ext(tc.changeName), func(t *testing.T) {
			if format == PayloadZstd {
				requireTools(t, "zstd")
			}
			dir := t.TempDir()
			base, delta, _, newTar := buildChainFixture(t, dir, format, tc.changeName)
			out := filepath.Join(dir, "out.mender")
			if err := testApplier().ApplyChain(context.Background(), base, []string{delta}, out, func(int, string) {}); err != nil {
				t.Fatal(err)
			}

			got, err := BasePayloadFormat(out)
			if err != nil || got != format {
				t.Fatalf("output format = %q, %v, want %q", got, err, format)
			}

			f, err := os.Open(out)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			tr := tar.NewReader(f)
			for {
				hdr, err := tr.Next()
				if err != nil {
					t.Fatal("output has no payload member")
				}
				if !strings.HasPrefix(hdr.Name, "data/0000.") {
					continue
				}
				raw, _ := io.ReadAll(tr)
				var plain []byte
				if format == PayloadZstd {
					d, _ := zstd.NewReader(bytes.NewReader(raw))
					defer d.Close()
					plain, err = io.ReadAll(d)
				} else {
					cmd := exec.Command("gzip", "-dc")
					cmd.Stdin = bytes.NewReader(raw)
					plain, err = cmd.Output()
				}
				if err != nil || !bytes.Equal(plain, newTar) {
					t.Fatalf("reconstructed payload differs (err %v)", err)
				}
				break
			}
		})
	}
}
