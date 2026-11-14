package delta

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// This optional integration test exercises the actual release generator and
// Mender signature verifier, rather than a synthetic delta metadata fixture.
func TestSignedMenderCompressionTransitionIntegration(t *testing.T) {
	tool := os.Getenv("MENDER_ARTIFACT")
	releaseDir := os.Getenv("LIBRESCOOT_RELEASE_DIR")
	if tool == "" || releaseDir == "" {
		t.Skip("set MENDER_ARTIFACT and LIBRESCOOT_RELEASE_DIR")
	}
	requireTools(t, "openssl", "python3", "gzip", "tar", "zstd", "xdelta3", "hdiffz", "hpatchz")
	dir := t.TempDir()
	run := func(name string, args ...string) {
		t.Helper()
		cmd := exec.Command(name, args...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, output)
		}
	}
	key, pub := filepath.Join(dir, "key.pem"), filepath.Join(dir, "pub.pem")
	run("openssl", "genpkey", "-algorithm", "RSA", "-pkeyopt", "rsa_keygen_bits:2048", "-out", key)
	run("openssl", "pkey", "-in", key, "-pubout", "-out", pub)
	oldRoot, newRoot := filepath.Join(dir, "old.ext4"), filepath.Join(dir, "new.ext4")
	oldBytes := bytes.Repeat([]byte("signed synthetic rootfs data "), 20000)
	if err := os.WriteFile(oldRoot, oldBytes, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newRoot, append(bytes.Clone(oldBytes), []byte("changed content")...), 0600); err != nil {
		t.Fatal(err)
	}
	oldArtifact, newGzip, newZstd := filepath.Join(dir, "old.mender"), filepath.Join(dir, "new-gzip.mender"), filepath.Join(dir, "new-zstd.mender")
	for _, item := range []struct{ root, output, version string }{{oldRoot, oldArtifact, "release-v1.4.3"}, {newRoot, newGzip, "release-v1.4.4"}} {
		run(tool, "write", "rootfs-image", "--no-progress", "-n", item.version, "-c", "unu-mdb", "-f", item.root, "-k", key, "-o", item.output)
	}
	run("python3", filepath.Join(releaseDir, "mender-recompress-zstd.py"), newGzip, newZstd, "--level", "3")
	run(tool, "read", "--no-progress", "-k", pub, newZstd)
	for _, algorithm := range []string{"xdelta3", "hdiffpatch"} {
		t.Run(algorithm, func(t *testing.T) {
			patchPath := filepath.Join(dir, algorithm+".delta")
			args := []string{filepath.Join(releaseDir, "mender-delta-create.py"), oldArtifact, newZstd, patchPath}
			if algorithm == "hdiffpatch" {
				args = append(args, "--hdiffpatch")
			}
			run("python3", args...)
			reconstructed := filepath.Join(dir, algorithm+".mender")
			if err := testApplier().ApplyChain(context.Background(), oldArtifact, []string{patchPath}, reconstructed, func(int, string) {}); err != nil {
				t.Fatal(err)
			}
			run(tool, "read", "--no-progress", "-k", pub, reconstructed)
			format, err := BasePayloadFormat(reconstructed)
			if err != nil || format != PayloadGzip {
				t.Fatalf("reconstructed base format %q: %v", format, err)
			}
		})
	}
}
