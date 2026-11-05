package catalog

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func tarData(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for n, data := range files {
		if err := w.WriteHeader(&tar.Header{Name: n, Mode: 0644, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
func zipData(t *testing.T, data []byte, stamp int64) []byte {
	t.Helper()
	var out bytes.Buffer
	w := gzip.NewWriter(&out)
	w.ModTime = time.Unix(stamp, 0)
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
func TestRealMenderArtifactIdentity(t *testing.T) {
	file := os.Getenv("OTA_TEST_MENDER_ARTIFACT")
	if file == "" {
		t.Skip("OTA_TEST_MENDER_ARTIFACT not set")
	}
	got, err := Inspect(context.Background(), file, "unu-mdb", true)
	if err != nil {
		t.Fatal(err)
	}
	if expected := os.Getenv("OTA_TEST_MENDER_ID"); expected == "" || got.ID != expected {
		t.Fatalf("identity = %s, want %s", got.ID, expected)
	}
}

func TestInspectAndVerify(t *testing.T) {
	data := []byte("rootfs content")
	raw := tarData(t, map[string][]byte{"rootfs.ext4": data})
	info, _ := json.Marshal(map[string]any{"artifact_provides": map[string]any{"artifact_name": "release-v1.4.1"}, "artifact_depends": map[string]any{"device_type": []string{"unu-mdb"}}})
	header := zipData(t, tarData(t, map[string][]byte{"header-info": info, "headers/0000/type-info": []byte(`{"artifact_provides":{"rootfs-image.librescoot-updater-protocol":"2"}}`)}), 0)
	manifest := []byte(Hash(data) + "  data/0000/rootfs.ext4\n")
	makeFile := func(stamp int64) string {
		p := filepath.Join(t.TempDir(), "image.mender")
		files := map[string][]byte{"version": []byte("{}"), "manifest": manifest, "header.tar.gz": header, "data/0000.tar.gz": zipData(t, raw, stamp)}
		if err := os.WriteFile(p, tarData(t, files), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	first := makeFile(1)
	second := makeFile(2)
	a, err := Inspect(context.Background(), first, "unu-mdb", true)
	if err != nil {
		t.Fatal(err)
	}
	if a.PayloadSHA256 != Hash(raw) || a.PayloadSize != int64(len(raw)) || a.UpdaterProtocol != 2 {
		t.Fatalf("wrong source payload: %+v", a)
	}
	if err := Verify(context.Background(), second, a); err != nil {
		t.Fatal(err)
	}
	b, err := Inspect(context.Background(), second, "unu-mdb", false)
	if err != nil || a.ID != b.ID {
		t.Fatalf("identity mismatch %v", err)
	}
	if _, err := Inspect(context.Background(), first, "unu-dbc", true); err == nil {
		t.Fatal("wrong board accepted")
	}
	a.PayloadSHA256 = Hash([]byte("wrong"))
	if Verify(context.Background(), first, a) == nil {
		t.Fatal("wrong payload accepted")
	}
}
