package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/librescoot/update-service/internal/catalog"
	"github.com/librescoot/update-service/internal/mender"
	"github.com/librescoot/update-service/internal/mender/delta"
)

type fixtureEntry struct {
	name string
	data []byte
}

func fixtureTar(t *testing.T, entries []fixtureEntry) []byte {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for _, e := range entries {
		if err := w.WriteHeader(&tar.Header{Name: e.name, Mode: 0644, Size: int64(len(e.data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(e.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func fixtureGzip(t *testing.T, b []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w := gzip.NewWriter(&out)
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
func fixtureImage(t *testing.T, dir, version string, root []byte) (string, []byte, []fixtureEntry, catalog.Artifact) {
	t.Helper()
	raw := fixtureTar(t, []fixtureEntry{{"rootfs.ext4", root}})
	hi, _ := json.Marshal(map[string]any{"artifact_provides": map[string]any{"artifact_name": "release-" + version}, "artifact_depends": map[string]any{"device_type": []string{"unu-mdb"}}})
	header := fixtureGzip(t, fixtureTar(t, []fixtureEntry{{"header-info", hi}}))
	manifest := []byte(catalog.Hash(root) + "  data/0000/rootfs.ext4\n")
	metadata := []fixtureEntry{{"version", []byte(`{"format":"mender","version":3}`)}, {"manifest", manifest}, {"manifest.sig", []byte("synthetic signature")}, {"header.tar.gz", header}}
	entries := append(append([]fixtureEntry(nil), metadata...), fixtureEntry{"data/0000.tar.gz", fixtureGzip(t, raw)})
	name := filepath.Join(dir, "librescoot-unu-mdb-"+version+".mender")
	if err := os.WriteFile(name, fixtureTar(t, entries), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := catalog.Inspect(context.Background(), name, "unu-mdb", true)
	if err != nil {
		t.Fatal(err)
	}
	return name, raw, metadata, a
}
func TestPythonCatalogRevisionGolden(t *testing.T) {
	fixture := `{"artifacts":[],"deltas":[],"revision":"bbc3b3c40bffd2ad296415cfbdfa5d3e106861ab752a62411f9c78ac39dd4086","schema_version":2,"targets":{"stable":{},"testing":{}}}`
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, fixture) }))
	defer srv.Close()
	api := NewGitHubAPI(context.Background(), srv.URL, log.New(io.Discard, "", 0))
	api.client = srv.Client()
	if _, err := api.getCatalogV2(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCandidateV2UsesExactLegacyTarget(t *testing.T) {
	u, _ := newTestUpdaterForPreview(t, nil)
	dir := t.TempDir()
	u.mender = mender.NewManager(dir, nil, u.logger)
	_, _, _, a := fixtureImage(t, dir, "v1.4.1", []byte("old"))
	_, _, _, b := fixtureImage(t, dir, "testing-20261105T120000", []byte("new"))
	for _, item := range []*catalog.Artifact{&a, &b} {
		item.Full = catalog.Object{URL: "https://example.org/librescoot-" + item.Variant + "-" + item.Version + ".mender", Size: 10000, SHA256: catalog.Hash([]byte(item.ID))}
	}
	edge := catalog.Edge{Source: a.ID, Target: b.ID, URL: "https://example.org/librescoot-unu-mdb-from-" + a.ID + "-to-" + b.ID + ".delta", Size: 10, SHA256: catalog.Hash([]byte("edge"))}
	c := catalog.Catalog{Schema: 2, Targets: map[string]map[string]string{"testing": {"unu-mdb": b.ID}}, Artifacts: []catalog.Artifact{a, b}, Deltas: []catalog.Edge{edge}}
	encoded, _ := json.Marshal(c)
	var canonical map[string]any
	if err := json.Unmarshal(encoded, &canonical); err != nil {
		t.Fatal(err)
	}
	delete(canonical, "revision")
	encoded, _ = json.Marshal(canonical)
	c.Revision = catalog.Hash(encoded)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(c) }))
	defer srv.Close()
	u.githubAPI = NewGitHubAPI(context.Background(), srv.URL, u.logger)
	u.githubAPI.client = srv.Client()
	r := Release{TagName: b.Version, Prerelease: true, Assets: []Asset{{Name: "librescoot-unu-mdb-" + b.Version + ".mender", URL: b.Full.URL, Size: b.Full.Size, Sha256: b.Full.SHA256}}}
	p, ok := u.candidateV2(context.Background(), "testing", "unu-mdb", a.Version, r, "delta")
	if !ok || p.Method() != "delta" || p.Base.ID != a.ID || p.Target.ID != b.ID {
		t.Fatalf("wrong plan: %+v, %v", p, ok)
	}
	r.TagName = "testing-20261106T120000"
	if _, ok := u.candidateV2(context.Background(), "testing", "unu-mdb", a.Version, r, "delta"); ok {
		t.Fatal("stale v2 target accepted")
	}
}

func TestCrossChannelDeltaReconstruction(t *testing.T) {
	if _, err := exec.LookPath("xdelta3"); err != nil {
		t.Skip("xdelta3 not installed")
	}
	dir := t.TempDir()
	old, oldRaw, _, a := fixtureImage(t, dir, "v1.4.1", bytes.Repeat([]byte("a"), 8192))
	_, newRaw, files, b := fixtureImage(t, dir, "testing-20261105T120000", append(bytes.Repeat([]byte("a"), 4096), bytes.Repeat([]byte("b"), 4096)...))
	oldFile, newFile, patchFile := filepath.Join(dir, "old.raw"), filepath.Join(dir, "new.raw"), filepath.Join(dir, "patch")
	if err := os.WriteFile(oldFile, oldRaw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newFile, newRaw, 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("xdelta3", "-e", "-s", oldFile, newFile, patchFile).CombinedOutput(); err != nil {
		t.Fatalf("xdelta: %v %s", err, output)
	}
	patch, err := os.ReadFile(patchFile)
	if err != nil {
		t.Fatal(err)
	}
	changes := map[string]delta.ChangeInfo{"data/0000.tar.gz": {Type: "modified", Patch: "payload.xdelta", OldMeta: delta.FileMeta{DecompressedSHA256: a.PayloadSHA256}, NewMeta: delta.FileMeta{DecompressedSHA256: b.PayloadSHA256}}}
	var entries []fixtureEntry
	for _, f := range files {
		changes[f.name] = delta.ChangeInfo{Type: "new"}
		entries = append(entries, fixtureEntry{"new_files/" + f.name, f.data})
	}
	meta := delta.DeltaMetadata{Version: 3, OldArtifactName: a.Name, NewArtifactName: b.Name, OldPayloadChecksum: a.RootfsSHA256, NewPayloadChecksum: b.RootfsSHA256, Changes: changes}
	data, _ := json.Marshal(meta)
	entries = append([]fixtureEntry{{"metadata.json", data}, {"patches/payload.xdelta", patch}}, entries...)
	patchArchive := filepath.Join(dir, "edge.delta")
	if err := os.WriteFile(patchArchive, fixtureGzip(t, fixtureTar(t, entries)), 0600); err != nil {
		t.Fatal(err)
	}
	p := catalog.Plan{Nodes: []catalog.Artifact{a, b}, Steps: []catalog.Edge{{Source: a.ID, Target: b.ID}}}
	if err := verifyV2Deltas(p, []string{patchArchive}); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "reconstructed.mender")
	applier := delta.NewApplier(log.New(io.Discard, "", 0), dir)
	if err := applier.ApplyChain(context.Background(), old, []string{patchArchive}, output, func(int, string) {}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Verify(context.Background(), output, b); err != nil {
		t.Fatal(err)
	}
}
