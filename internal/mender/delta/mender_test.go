package delta

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type payloadFile struct {
	name string
	data []byte
}

// writePayloadTar builds a payload tar in the requested format. A name over
// 100 bytes pushes pax and GNU into emitting their extra leading member, which
// is the layout a hand-rolled header parse reads as the payload's own header.
func writePayloadTar(t *testing.T, path string, format tar.Format, files []payloadFile) {
	t.Helper()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		hdr := &tar.Header{
			Name:     f.name,
			Mode:     0644,
			Size:     int64(len(f.data)),
			Typeflag: tar.TypeReg,
			Format:   format,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %s: %v", f.name, err)
		}
		if _, err := tw.Write(f.data); err != nil {
			t.Fatalf("write body %s: %v", f.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// firstBlockTypeflag returns the typeflag byte of the archive's very first
// 512-byte header block, which is the block a hand-rolled parse reads as the
// payload's own header. '0' is a regular file, 'x' a pax extended header, 'L'
// a GNU long name.
func firstBlockTypeflag(t *testing.T, path string) byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(data) < 512 {
		t.Fatalf("%s is shorter than one tar block", path)
	}
	return data[156]
}

// assertVerbatim checks the compressed output decompresses back to the input
// byte for byte. gzip is deterministic for a given input and level, so this is
// what makes the output identical to streaming the file straight through.
func assertVerbatim(t *testing.T, srcPath, gzPath string) {
	t.Helper()

	want, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	f, err := os.Open(gzPath)
	if err != nil {
		t.Fatalf("open compressed: %v", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("open gzip: %v", err)
	}
	defer gz.Close()

	got, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("decompress: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("round trip differs: got %d bytes, want %d", len(got), len(want))
	}
}

func TestCompressPayloadAndHash(t *testing.T) {
	rootfs := bytes.Repeat([]byte("rootfs-content-"), 5000)
	want := sha256.Sum256(rootfs)

	// No separator, so ustar cannot split this across its prefix and name
	// fields and both formats have to reach for their extension mechanism.
	// A long path *with* separators would quietly fit a plain ustar header.
	longName := strings.Repeat("a", 150) + ".ext4"

	tests := []struct {
		name     string
		format   tar.Format
		inner    string
		wantFlag byte // typeflag of the archive's first header block
	}{
		{name: "ustar", format: tar.FormatUSTAR, inner: "rootfs.ext4", wantFlag: tar.TypeReg},
		{name: "pax long name", format: tar.FormatPAX, inner: longName, wantFlag: tar.TypeXHeader},
		{name: "gnu long name", format: tar.FormatGNU, inner: longName, wantFlag: tar.TypeGNULongName},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "payload.tar")
			out := filepath.Join(dir, "payload.tar.gz")
			writePayloadTar(t, src, tc.format, []payloadFile{{name: tc.inner, data: rootfs}})

			// Guard the fixture. Without this an archive that quietly came
			// out as plain ustar would pass while exercising nothing.
			if got := firstBlockTypeflag(t, src); got != tc.wantFlag {
				t.Fatalf("first block typeflag = %q, want %q", got, tc.wantFlag)
			}

			got, err := CompressPayloadAndHash(src, out, newProgressTracker(0, nil))
			if err != nil {
				t.Fatalf("CompressPayloadAndHash: %v", err)
			}
			if got != hex.EncodeToString(want[:]) {
				t.Errorf("checksum = %s, want %s", got, hex.EncodeToString(want[:]))
			}
			assertVerbatim(t, src, out)
		})
	}
}

// The rootfs checksum is the first member's, and later members still reach
// gzip untouched.
func TestCompressPayloadAndHashUsesFirstMember(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "payload.tar")
	out := filepath.Join(dir, "payload.tar.gz")

	rootfs := bytes.Repeat([]byte("first-"), 3000)
	trailer := bytes.Repeat([]byte("second-"), 2000)
	writePayloadTar(t, src, tar.FormatUSTAR, []payloadFile{
		{name: "rootfs.ext4", data: rootfs},
		{name: "extra.bin", data: trailer},
	})

	got, err := CompressPayloadAndHash(src, out, newProgressTracker(0, nil))
	if err != nil {
		t.Fatalf("CompressPayloadAndHash: %v", err)
	}
	want := sha256.Sum256(rootfs)
	if got != hex.EncodeToString(want[:]) {
		t.Errorf("checksum = %s, want the first member's %s", got, hex.EncodeToString(want[:]))
	}
	assertVerbatim(t, src, out)
}

func TestCompressPayloadAndHashEmptyTar(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "payload.tar")
	writePayloadTar(t, src, tar.FormatUSTAR, nil)

	if _, err := CompressPayloadAndHash(src, filepath.Join(dir, "out.gz"), newProgressTracker(0, nil)); err == nil {
		t.Fatal("expected an error for a payload tar with no members")
	}
}

func TestCompressPayloadAndHashMissingInput(t *testing.T) {
	dir := t.TempDir()
	if _, err := CompressPayloadAndHash(filepath.Join(dir, "nope.tar"), filepath.Join(dir, "out.gz"), newProgressTracker(0, nil)); err == nil {
		t.Fatal("expected an error for a payload that does not exist")
	}
}
