package delta

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func signingCommand(t *testing.T, name string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return out
}

func disposableSigningKey(t *testing.T, dir, name string) (private, public string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	privDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	private, public = filepath.Join(dir, name+".key"), filepath.Join(dir, name+".pub")
	for path, block := range map[string]*pem.Block{
		private: {Type: "EC PRIVATE KEY", Bytes: privDER},
		public:  {Type: "PUBLIC KEY", Bytes: pubDER},
	} {
		if err := os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return private, public
}

func artifactMembers(t *testing.T, path string) (map[string][]byte, []string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	members := make(map[string][]byte)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return members, names
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
		members[hdr.Name], err = io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
	}
}

// The external tools are opt-in so ordinary unit tests need no sibling checkout
// or downloaded binaries. Explicit configuration is strict: missing tools fail.
func TestSignedDeltaIntegration(t *testing.T) {
	tool, creator := os.Getenv("MENDER_ARTIFACT"), os.Getenv("MENDER_DELTA_CREATOR")
	if tool == "" && creator == "" && os.Getenv("MENDER_UPDATE") == "" {
		t.Skip("set MENDER_ARTIFACT and MENDER_DELTA_CREATOR to run signing integration")
	}
	if tool == "" || creator == "" {
		t.Fatal("both MENDER_ARTIFACT and MENDER_DELTA_CREATOR are required")
	}
	for _, bin := range []string{tool, "python3", "xdelta3", "tar", "gzip"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(creator); err != nil {
		t.Fatal(err)
	}
	t.Logf("verifier: %s", signingCommand(t, tool, "--version"))
	if client := os.Getenv("MENDER_UPDATE"); client != "" {
		t.Logf("client: %s", signingCommand(t, client, "--version"))
	}
	dir := t.TempDir()
	key, pub := disposableSigningKey(t, dir, "release")
	nextKey, nextPub := disposableSigningKey(t, dir, "replacement")

	makeArtifact := func(name, signingKey string, payload []byte) string {
		t.Helper()
		rootfs := filepath.Join(dir, name+".ext4")
		if err := os.WriteFile(rootfs, payload, 0600); err != nil {
			t.Fatal(err)
		}
		unsigned := filepath.Join(dir, name+"-unsigned.mender")
		signingCommand(t, tool, "write", "rootfs-image", "-t", "signing-test", "-n", name,
			"-f", rootfs, "--compression", "gzip", "-o", unsigned)
		if signingKey == "" {
			return unsigned
		}
		signed := filepath.Join(dir, name+".mender")
		signingCommand(t, tool, "sign", unsigned, "-k", signingKey, "-o", signed)
		return signed
	}
	verify := func(t *testing.T, path, public string, valid bool) {
		t.Helper()
		args := []string{"validate"}
		if public != "" {
			args = append(args, "-k", public)
		}
		args = append(args, path)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		out, err := exec.CommandContext(ctx, tool, args...).CombinedOutput()
		if (err == nil) != valid || ctx.Err() != nil {
			t.Fatalf("validate %s (valid=%t): %v\n%s", path, valid, err, out)
		}
		verifySigningTestClient(t, path, public, valid)
	}
	payload := bytes.Repeat([]byte("disposable rootfs fixture\n"), 4096)
	a := makeArtifact("a", "", payload)
	b := makeArtifact("b", key, append(bytes.Clone(payload), 'b'))
	c := makeArtifact("c", key, append(bytes.Clone(payload), 'c'))
	// Same payload, new signing key: only metadata/signature changes.
	d := makeArtifact("d", nextKey, append(bytes.Clone(payload), 'c'))
	u := makeArtifact("unsigned", "", append(bytes.Clone(payload), 'u'))
	verify(t, a, "", true)
	verify(t, a, pub, false)
	verify(t, b, pub, true)
	verify(t, c, pub, true)
	verify(t, d, nextPub, true)
	verify(t, d, pub, false)

	delta := func(name, base, target string) string {
		t.Helper()
		path := filepath.Join(dir, name+".delta")
		signingCommand(t, "python3", creator, base, target, path)
		return path
	}
	ab, bc, cd, cu := delta("ab", a, b), delta("bc", b, c), delta("cd", c, d), delta("cu", c, u)
	au := delta("au", a, u)

	apply := func(t *testing.T, name, base string, deltas []string, target, public string) string {
		t.Helper()
		out := filepath.Join(dir, name+".mender")
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		applier := NewApplier(log.New(io.Discard, "", 0), dir)
		if err := applier.ApplyChain(ctx, base, deltas, out, func(int, string) {}); err != nil {
			t.Fatal(err)
		}
		verify(t, out, public, true)
		got, gotNames := artifactMembers(t, out)
		want, wantNames := artifactMembers(t, target)
		if !reflect.DeepEqual(gotNames, wantNames) {
			t.Fatalf("archive order = %v, want %v", gotNames, wantNames)
		}
		for _, name := range []string{"version", "manifest", "manifest.sig", "header.tar.gz"} {
			if !bytes.Equal(got[name], want[name]) {
				t.Fatalf("%s changed in reconstruction", name)
			}
		}
		return out
	}
	var reconstructed string
	t.Run("unsigned to unsigned", func(t *testing.T) { apply(t, "out-au", a, []string{au}, u, "") })
	t.Run("unsigned to signed", func(t *testing.T) { apply(t, "out-ab", a, []string{ab}, b, pub) })
	t.Run("signed to signed", func(t *testing.T) { reconstructed = apply(t, "out-bc", b, []string{bc}, c, pub) })
	t.Run("multi hop", func(t *testing.T) { apply(t, "out-abc", a, []string{ab, bc}, c, pub) })
	t.Run("replacement key metadata only", func(t *testing.T) { apply(t, "out-cd", c, []string{cd}, d, nextPub) })
	t.Run("recompressed base next hop", func(t *testing.T) {
		if reconstructed == "" {
			t.Fatal("missing reconstructed base")
		}
		apply(t, "out-bcd", reconstructed, []string{cd}, d, nextPub)
	})
	t.Run("multi hop replacement key", func(t *testing.T) { apply(t, "out-abcd", a, []string{ab, bc, cd}, d, nextPub) })
	t.Run("unsigned target has no stale signature", func(t *testing.T) {
		out := apply(t, "out-cu", c, []string{cu}, u, "")
		verify(t, out, pub, false)
	})

	t.Run("cancelled chain preserves existing output", func(t *testing.T) {
		out := filepath.Join(dir, "cancelled.mender")
		if err := os.WriteFile(out, []byte("previous output"), 0600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		applier := NewApplier(log.New(io.Discard, "", 0), dir)
		err := applier.ApplyChain(ctx, a, []string{ab, bc}, out, func(int, string) {})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled apply: %v", err)
		}
		got, err := os.ReadFile(out)
		if err != nil || string(got) != "previous output" {
			t.Fatalf("existing output changed: %q, %v", got, err)
		}
		leftovers, err := filepath.Glob(filepath.Join(dir, "delta-chain-*"))
		if err != nil || len(leftovers) != 0 {
			t.Fatalf("work directories remain: %v, %v", leftovers, err)
		}
	})

	// Repacking preserves signatures, not verification policy. The standard
	// verifier must reject stripped signatures and altered signed content.
	original, order := artifactMembers(t, c)
	other, _ := artifactMembers(t, b)
	for _, change := range []string{"missing signature", "wrong signature", "manifest", "header", "payload"} {
		t.Run(change, func(t *testing.T) {
			var entries []tarEntry
			for _, name := range order {
				body := original[name]
				switch {
				case change == "missing signature" && name == "manifest.sig":
					continue
				case change == "wrong signature" && name == "manifest.sig":
					body = other[name]
				case change == "manifest" && name == "manifest":
					body = other[name]
				case change == "header" && name == "header.tar.gz":
					body = other[name]
				case change == "payload" && name == "data/0000.tar.gz":
					body = other[name]
				}
				entries = append(entries, tarEntry{name: name, body: body})
			}
			mutated := writeTar(t, filepath.Join(t.TempDir(), "tampered.mender"), false, entries)
			verify(t, mutated, pub, false)
		})
	}
}
