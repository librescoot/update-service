package updater

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"github.com/librescoot/update-service/internal/config"
	"github.com/librescoot/update-service/internal/inhibitor"
	"github.com/librescoot/update-service/internal/mender"
	"github.com/librescoot/update-service/internal/mender/delta"
	"github.com/librescoot/update-service/internal/power"
	"github.com/librescoot/update-service/internal/redis"
	"github.com/librescoot/update-service/internal/status"
)

// newStagedTestUpdater wires just enough of an Updater to drive the local
// delta/full-image paths: a real mender.Manager on a temp download dir, a
// status reporter backed by miniredis, and an installArtifact stub so the tests
// can assert whether Mender was asked to write anything.
func newStagedTestUpdater(t *testing.T, component string) (*Updater, *miniredis.Miniredis, *[]string) {
	t.Helper()
	mr := miniredis.RunT(t)
	rc, err := redis.New(mr.Addr())
	if err != nil {
		t.Fatalf("connecting test redis client: %v", err)
	}
	t.Cleanup(func() { _ = rc.Close() })

	logger := log.New(io.Discard, "", 0)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	installs := &[]string{}
	downloadDir := t.TempDir()
	// The default applier is the real one, so tests that expect its refusal
	// behaviour are unchanged; tests that need a chain to succeed override
	// applyDeltaChain with a stub.
	mgr := mender.NewManager(t.TempDir(), func() mender.Budget { return mender.Budget{} }, logger)
	u := &Updater{
		config:    &config.Config{Component: component, DownloadDir: downloadDir, DryRun: true},
		redis:     rc,
		status:    status.NewReporter(rc.GetClient(), component, logger),
		mender:    mgr,
		inhibitor: inhibitor.New(rc.GetClient(), logger),
		power:     power.New(rc.GetClient(), logger),
		installArtifact: func(path string, progress mender.InstallProgressCallback) error {
			*installs = append(*installs, path)
			return nil
		},
		applyDeltaChain: mgr.ApplyDownloadedDeltaChain,
		logger:          logger,
		ctx:             ctx,
		cancel:          cancel,
	}
	// Registered after the client, so it runs first (t.Cleanup is LIFO): the
	// heartbeat goroutine a file install starts must finish before the client
	// it writes through is closed.
	t.Cleanup(func() { u.wg.Wait() })
	return u, mr, installs
}

// writeBaseMender fabricates the minimal artifact the base check reads: a tar
// whose manifest names the rootfs payload checksum.
func writeBaseMender(t *testing.T, dir, name, rootfsChecksum string) string {
	t.Helper()
	manifest := rootfsChecksum + "  data/0000/rootfs.ext4\n"
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "manifest", Mode: 0644, Size: int64(len(manifest))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(manifest)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeDeltaFile fabricates a delta carrying only the metadata the chain
// resolution reads. It is deliberately not a real patch: nothing here should
// reach the point of applying bytes.
func writeDeltaFile(t *testing.T, dir, name, oldChecksum, newChecksum string) string {
	t.Helper()
	meta, err := json.Marshal(delta.DeltaMetadata{
		OldPayloadChecksum: oldChecksum,
		NewPayloadChecksum: newChecksum,
		Version:            3,
	})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "metadata.json", Mode: 0644, Size: int64(len(meta))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(meta); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestValidateDeltaChain(t *testing.T) {
	cases := []struct {
		name     string
		targets  []string
		current  string
		wantBase string
		wantErr  string // substring; "" means success
	}{
		{"single nightly link", []string{"nightly-20260102T000000"}, "nightly-20260101T000000", "nightly-20260101t000000", ""},
		{"two nightly links in order", []string{"nightly-20260102T000000", "nightly-20260103T000000"}, "nightly-20260101T000000", "nightly-20260101t000000", ""},
		{"three stable links in order", []string{"v1.1.0", "v1.2.0", "v1.3.0"}, "1.0.0", "v1.0.0", ""},
		{"links out of order", []string{"nightly-20260103T000000", "nightly-20260102T000000"}, "nightly-20260101T000000", "", "not newer"},
		{"duplicate link", []string{"nightly-20260102T000000", "nightly-20260102T000000"}, "nightly-20260101T000000", "", "not newer"},
		{"first link not newer than installed", []string{"nightly-20260101T000000", "nightly-20260102T000000"}, "nightly-20260101T000000", "", "not newer"},
		{"channel changes mid-chain", []string{"nightly-20260102T000000", "v1.3.0"}, "nightly-20260101T000000", "", "channel"},
		{"unparseable second link", []string{"nightly-20260102T000000", ""}, "nightly-20260101T000000", "", "cannot parse"},
		{"empty chain", nil, "nightly-20260101T000000", "", "empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, err := validateDeltaChain(tc.targets, tc.current)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if base != tc.wantBase {
					t.Errorf("base = %q, want %q", base, tc.wantBase)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil (base=%q)", tc.wantErr, base)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestPlanStagedArtifacts covers the discovery rules one by one against the
// permanent contents of the component download dir (the running version's base
// .mender always sits there).
func TestPlanStagedArtifacts(t *testing.T) {
	const running = "nightly-20260101T000000"
	base := "librescoot-unu-mdb-nightly-20260101T000000.mender"
	d1 := "librescoot-unu-mdb-nightly-20260102T000000.delta"
	d2 := "librescoot-unu-mdb-nightly-20260103T000000.delta"
	newer := "librescoot-unu-mdb-nightly-20260104T000000.mender"

	cases := []struct {
		name     string
		menders  []string
		deltas   []string
		wantFull string
		wantDelt []string
		wantErr  string
	}{
		{
			name:    "base image alone is ignored",
			menders: []string{base},
			wantErr: "no staged artifact",
		},
		{
			name:     "base image and a newer full image",
			menders:  []string{base, newer},
			wantFull: newer,
		},
		{
			name:     "base image and a delta chain",
			menders:  []string{base},
			deltas:   []string{d2, d1},
			wantDelt: []string{d1, d2},
		},
		{
			name:    "newer full image together with a delta",
			menders: []string{base, newer},
			deltas:  []string{d1},
			wantErr: "staged together",
		},
		{
			name:    "two newer full images",
			menders: []string{base, newer, "librescoot-unu-mdb-nightly-20260105T000000.mender"},
			wantErr: "ambiguous which one",
		},
		{
			name:    "old delta is stale and ignored",
			menders: []string{base},
			deltas:  []string{"librescoot-unu-mdb-nightly-20251231T000000.delta"},
			wantErr: "no staged artifact",
		},
		{
			// validateDeltaTarget rejects a delta on another channel (it is
			// not a valid successor of a nightly install), so update-service
			// ignores it. UMS refuses the whole board before staging when a
			// drop spans channels, so this is only a defensive second line.
			name:     "delta on another channel is ignored",
			menders:  []string{base},
			deltas:   []string{d1, "librescoot-unu-mdb-v1.2.0.delta"},
			wantDelt: []string{d1},
		},
		{
			// A leftover cross-channel full image must not be ranked against
			// the running version: Compare is lexicographic across channels,
			// so a testing-… mender would compare as newer than a nightly-…
			// install and be installed as a cross-channel image.
			name:     "cross-channel newer full image is ignored",
			menders:  []string{base, "librescoot-unu-mdb-testing-20260105T000000.mender"},
			deltas:   []string{d1},
			wantDelt: []string{d1},
		},
		{
			name:    "cross-channel full image alone is not staged",
			menders: []string{base, "librescoot-unu-mdb-testing-20260105T000000.mender"},
			wantErr: "no staged artifact",
		},
		{
			// An unparsable delta name (a manual/BLE leftover) is ignored like
			// an unparsable .mender, so it cannot refuse a legitimate chain
			// staged beside it.
			name:     "unparsable delta name is ignored",
			menders:  []string{base},
			deltas:   []string{"update.delta", d1},
			wantDelt: []string{d1},
		},
		{
			name:    "delta with unknown channel prefix is ignored",
			menders: []string{base},
			deltas:  []string{"librescoot-unu-mdb-foo-bar.delta"},
			wantErr: "no staged artifact",
		},
		{
			// A junk delta must not block a legitimate staged image: only real
			// delta candidates can make a full-image-plus-delta set ambiguous.
			name:     "newer full image beside an unparsable delta installs the image",
			menders:  []string{base, newer},
			deltas:   []string{"update.delta"},
			wantFull: newer,
		},
		{
			name:     "newer full image beside an unknown-channel delta installs the image",
			menders:  []string{base, newer},
			deltas:   []string{"librescoot-unu-mdb-foo-bar.delta"},
			wantFull: newer,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := planStagedArtifacts(running, tc.menders, tc.deltas)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got plan %+v", tc.wantErr, plan)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if plan.fullImage != tc.wantFull {
				t.Errorf("fullImage = %q, want %q", plan.fullImage, tc.wantFull)
			}
			if strings.Join(plan.deltas, ",") != strings.Join(tc.wantDelt, ",") {
				t.Errorf("deltas = %v, want %v", plan.deltas, tc.wantDelt)
			}
		})
	}
}

// TestResolveStagedDeltaChain pins the checksum-based chain resolution: order
// comes from the metadata, and a fork or an unplaceable delta refuses the whole
// set.
func TestResolveStagedDeltaChain(t *testing.T) {
	newManager := func(t *testing.T) (*mender.Manager, string) {
		t.Helper()
		dir := t.TempDir()
		return mender.NewManager(dir, func() mender.Budget { return mender.Budget{} }, log.New(io.Discard, "", 0)), dir
	}

	t.Run("resolves by metadata order", func(t *testing.T) {
		m, dir := newManager(t)
		const baseVersion = "nightly-20260101t000000"
		writeBaseMender(t, dir, "librescoot-unu-mdb-nightly-20260101T000000.mender", strings.Repeat("a", 64))
		d1 := writeDeltaFile(t, dir, "librescoot-unu-mdb-nightly-20260102T000000.delta", strings.Repeat("a", 64), strings.Repeat("b", 64))
		d2 := writeDeltaFile(t, dir, "librescoot-unu-mdb-nightly-20260103T000000.delta", strings.Repeat("b", 64), strings.Repeat("c", 64))

		got, err := m.ResolveStagedDeltaChain([]string{d2, d1}, baseVersion)
		if err != nil {
			t.Fatalf("ResolveStagedDeltaChain: %v", err)
		}
		if len(got) != 2 || got[0] != d1 || got[1] != d2 {
			t.Fatalf("chain = %v, want [%s %s]", got, d1, d2)
		}
	})

	t.Run("fork refuses the set", func(t *testing.T) {
		m, dir := newManager(t)
		const baseVersion = "nightly-20260101t000000"
		writeBaseMender(t, dir, "librescoot-unu-mdb-nightly-20260101T000000.mender", strings.Repeat("a", 64))
		d1 := writeDeltaFile(t, dir, "librescoot-unu-mdb-nightly-20260102T000000.delta", strings.Repeat("a", 64), strings.Repeat("b", 64))
		d2 := writeDeltaFile(t, dir, "librescoot-unu-mdb-nightly-20260103T000000.delta", strings.Repeat("a", 64), strings.Repeat("c", 64))

		_, err := m.ResolveStagedDeltaChain([]string{d1, d2}, baseVersion)
		if err == nil || !strings.Contains(err.Error(), "same base image") {
			t.Fatalf("err = %v, want a fork refusal", err)
		}
		if !isErrStagedChainAmbiguous(err) {
			t.Fatalf("err = %v, want ErrStagedChainAmbiguous", err)
		}
	})

	t.Run("delta that cannot be placed refuses the set", func(t *testing.T) {
		m, dir := newManager(t)
		const baseVersion = "nightly-20260101t000000"
		writeBaseMender(t, dir, "librescoot-unu-mdb-nightly-20260101T000000.mender", strings.Repeat("a", 64))
		d1 := writeDeltaFile(t, dir, "librescoot-unu-mdb-nightly-20260102T000000.delta", strings.Repeat("a", 64), strings.Repeat("b", 64))
		orphan := writeDeltaFile(t, dir, "librescoot-unu-mdb-nightly-20260103T000000.delta", strings.Repeat("x", 64), strings.Repeat("y", 64))

		_, err := m.ResolveStagedDeltaChain([]string{d1, orphan}, baseVersion)
		if err == nil || !strings.Contains(err.Error(), "do not fit the chain") {
			t.Fatalf("err = %v, want an unplaceable-delta refusal", err)
		}
		if !isErrStagedChainAmbiguous(err) {
			t.Fatalf("err = %v, want ErrStagedChainAmbiguous", err)
		}
	})

	t.Run("single delta passes through", func(t *testing.T) {
		m, dir := newManager(t)
		d1 := writeDeltaFile(t, dir, "librescoot-unu-mdb-nightly-20260102T000000.delta", strings.Repeat("a", 64), strings.Repeat("b", 64))
		got, err := m.ResolveStagedDeltaChain([]string{d1}, "nightly-20260101t000000")
		if err != nil || len(got) != 1 || got[0] != d1 {
			t.Fatalf("ResolveStagedDeltaChain = %v, %v; want [%s]", got, err, d1)
		}
	})

	t.Run("candidate without payload checksums refuses the set", func(t *testing.T) {
		m, dir := newManager(t)
		const baseVersion = "nightly-20260101t000000"
		writeBaseMender(t, dir, "librescoot-unu-mdb-nightly-20260101T000000.mender", strings.Repeat("a", 64))
		d1 := writeDeltaFile(t, dir, "librescoot-unu-mdb-nightly-20260102T000000.delta", strings.Repeat("a", 64), strings.Repeat("b", 64))
		// Predates the payload-checksum fields: cannot be placed by checksum.
		d2 := writeDeltaFile(t, dir, "librescoot-unu-mdb-nightly-20260103T000000.delta", "", "")

		_, err := m.ResolveStagedDeltaChain([]string{d1, d2}, baseVersion)
		if err == nil || !strings.Contains(err.Error(), "no payload checksums") {
			t.Fatalf("err = %v, want a no-payload-checksums refusal", err)
		}
		if !isErrStagedChainAmbiguous(err) {
			t.Fatalf("err = %v, want ErrStagedChainAmbiguous", err)
		}
	})

	t.Run("unreadable base manifest refuses the set", func(t *testing.T) {
		m, dir := newManager(t)
		// Not a tar: the base rootfs checksum cannot be read from it.
		if err := os.WriteFile(filepath.Join(dir, "librescoot-unu-mdb-nightly-20260101T000000.mender"), []byte("not a tar"), 0644); err != nil {
			t.Fatal(err)
		}
		d1 := writeDeltaFile(t, dir, "librescoot-unu-mdb-nightly-20260102T000000.delta", strings.Repeat("a", 64), strings.Repeat("b", 64))
		d2 := writeDeltaFile(t, dir, "librescoot-unu-mdb-nightly-20260103T000000.delta", strings.Repeat("b", 64), strings.Repeat("c", 64))

		_, err := m.ResolveStagedDeltaChain([]string{d1, d2}, "nightly-20260101t000000")
		if err == nil || !strings.Contains(err.Error(), "base rootfs checksum") {
			t.Fatalf("err = %v, want a base-checksum refusal", err)
		}
		if !isErrStagedChainAmbiguous(err) {
			t.Fatalf("err = %v, want ErrStagedChainAmbiguous", err)
		}
	})
}

func isErrStagedChainAmbiguous(err error) bool {
	return err != nil && strings.Contains(err.Error(), mender.ErrStagedChainAmbiguous.Error())
}

func TestApplyLocalDeltaChainBaseMismatchDoesNotInstall(t *testing.T) {
	u, mr, installs := newStagedTestUpdater(t, "mdb")
	dir := u.mender.GetDownloadDir()
	mr.HSet("version:mdb", "version_id", "nightly-20260101T000000")

	writeBaseMender(t, dir, "librescoot-unu-mdb-nightly-20260101T000000.mender", strings.Repeat("a", 64))
	// Both links verify against each other, but the first was not built from
	// the installed image.
	d1 := writeDeltaFile(t, dir, "librescoot-unu-mdb-nightly-20260102T000000.delta", strings.Repeat("b", 64), strings.Repeat("c", 64))
	d2 := writeDeltaFile(t, dir, "librescoot-unu-mdb-nightly-20260103T000000.delta", strings.Repeat("c", 64), strings.Repeat("d", 64))

	u.applyLocalDeltaChainLocked([]string{d1, d2}, "")

	waitForField(t, mr, "ota", "error:mdb", "delta-base-mismatch")
	if len(*installs) != 0 {
		t.Fatalf("install ran despite a base mismatch: %v", *installs)
	}
}

// TestApplyLocalDeltaChainOfOneBaseMismatch pins the README's claim that a
// chain whose base does not match is refused as delta-base-mismatch even when
// the chain is a single delta.
func TestApplyLocalDeltaChainOfOneBaseMismatch(t *testing.T) {
	u, mr, installs := newStagedTestUpdater(t, "mdb")
	dir := u.mender.GetDownloadDir()
	mr.HSet("version:mdb", "version_id", "nightly-20260101T000000")

	writeBaseMender(t, dir, "librescoot-unu-mdb-nightly-20260101T000000.mender", strings.Repeat("a", 64))
	d1 := writeDeltaFile(t, dir, "librescoot-unu-mdb-nightly-20260102T000000.delta", strings.Repeat("b", 64), strings.Repeat("c", 64))

	u.applyLocalDeltaChainLocked([]string{d1}, "")

	waitForField(t, mr, "ota", "error:mdb", "delta-base-mismatch")
	if len(*installs) != 0 {
		t.Fatalf("install ran despite a base mismatch: %v", *installs)
	}
}

func TestApplyLocalDeltaChainNoBaseImageDoesNotInstall(t *testing.T) {
	u, mr, installs := newStagedTestUpdater(t, "mdb")
	dir := u.mender.GetDownloadDir()
	mr.HSet("version:mdb", "version_id", "nightly-20260101T000000")

	// No .mender for the running version in the download dir.
	d1 := writeDeltaFile(t, dir, "librescoot-unu-mdb-nightly-20260102T000000.delta", strings.Repeat("a", 64), strings.Repeat("b", 64))
	d2 := writeDeltaFile(t, dir, "librescoot-unu-mdb-nightly-20260103T000000.delta", strings.Repeat("b", 64), strings.Repeat("c", 64))

	u.applyLocalDeltaChainLocked([]string{d1, d2}, "")

	waitForField(t, mr, "ota", "error:mdb", "no-base-image")
	if len(*installs) != 0 {
		t.Fatalf("install ran without a base image: %v", *installs)
	}
}

func TestApplyLocalDeltaChainEmptyInputDoesNotInstall(t *testing.T) {
	u, mr, installs := newStagedTestUpdater(t, "mdb")

	u.applyLocalDeltaChainLocked(nil, "")

	waitForField(t, mr, "ota", "error:mdb", "delta-rejected")
	if len(*installs) != 0 {
		t.Fatalf("install ran for an empty chain: %v", *installs)
	}
}

func TestApplyLocalDeltaChainNonDeltaMemberDoesNotInstall(t *testing.T) {
	u, mr, installs := newStagedTestUpdater(t, "mdb")
	dir := u.mender.GetDownloadDir()
	mr.HSet("version:mdb", "version_id", "nightly-20260101T000000")

	d1 := writeDeltaFile(t, dir, "librescoot-unu-mdb-nightly-20260102T000000.delta", strings.Repeat("a", 64), strings.Repeat("b", 64))
	full := writeBaseMender(t, dir, "librescoot-unu-mdb-nightly-20260103T000000.mender", strings.Repeat("c", 64))

	u.applyLocalDeltaChainLocked([]string{d1, full}, "")

	waitForField(t, mr, "ota", "error:mdb", "invalid-file")
	if len(*installs) != 0 {
		t.Fatalf("install ran for a chain containing a full image: %v", *installs)
	}
}

func TestApplyLocalDeltaChainOrderedReachesApplier(t *testing.T) {
	u, mr, installs := newStagedTestUpdater(t, "mdb")
	dir := u.mender.GetDownloadDir()
	mr.HSet("version:mdb", "version_id", "nightly-20260101T000000")

	baseSum := strings.Repeat("a", 64)
	writeBaseMender(t, dir, "librescoot-unu-mdb-nightly-20260101T000000.mender", baseSum)
	d1 := writeDeltaFile(t, dir, "librescoot-unu-mdb-nightly-20260102T000000.delta", baseSum, strings.Repeat("b", 64))
	d2 := writeDeltaFile(t, dir, "librescoot-unu-mdb-nightly-20260103T000000.delta", strings.Repeat("b", 64), strings.Repeat("c", 64))

	u.applyLocalDeltaChainLocked([]string{d1, d2}, "")

	// The chain was accepted: both the ordered targets and the base link
	// verified, and the applier was invoked with the final target published.
	waitForField(t, mr, "ota", "update-version:mdb", "nightly-20260103t000000")
	if got := mr.HGet("ota", "update-method:mdb"); got != "delta" {
		t.Errorf("update-method:mdb = %q, want delta", got)
	}
	// These fabricated artifacts cannot actually be applied, so the failure
	// must come from application rather than from rejection or base mismatch.
	waitForField(t, mr, "ota", "error:mdb", "delta-apply-failed")
	if len(*installs) != 0 {
		t.Fatalf("install ran for a chain that could not be applied: %v", *installs)
	}
}

// TestApplyLocalDeltaChainFallsBackWhenSeamUnset pins the nil-seam fallback:
// an Updater built directly by another test, without the injected applier, must
// reach the real mender applier rather than panicking on a nil seam.
func TestApplyLocalDeltaChainFallsBackWhenSeamUnset(t *testing.T) {
	u, mr, installs := newStagedTestUpdater(t, "mdb")
	u.applyDeltaChain = nil
	dir := u.mender.GetDownloadDir()
	mr.HSet("version:mdb", "version_id", "nightly-20260101T000000")

	baseSum := strings.Repeat("a", 64)
	writeBaseMender(t, dir, "librescoot-unu-mdb-nightly-20260101T000000.mender", baseSum)
	d1 := writeDeltaFile(t, dir, "librescoot-unu-mdb-nightly-20260102T000000.delta", baseSum, strings.Repeat("b", 64))

	u.applyLocalDeltaChainLocked([]string{d1}, "")

	// The real applier ran (and refused the fabricated delta) instead of a nil
	// call panicking the process.
	waitForField(t, mr, "ota", "error:mdb", "delta-apply-failed")
	if len(*installs) != 0 {
		t.Fatalf("install ran for a chain that could not be applied: %v", *installs)
	}
}

// TestHandleApplyStagedUpdatesInstallsNewerFullImage is the end-to-end
// discovery case: the running version's base .mender is present and ignored,
// and the single newer .mender is installed once.
func TestHandleApplyStagedUpdatesInstallsNewerFullImage(t *testing.T) {
	u, mr, installs := newStagedTestUpdater(t, "mdb")
	dir := u.mender.GetDownloadDir()
	mr.HSet("version:mdb", "version_id", "nightly-20260101T000000")

	writeBaseMender(t, dir, "librescoot-unu-mdb-nightly-20260101T000000.mender", strings.Repeat("a", 64))
	newer := writeBaseMender(t, dir, "librescoot-unu-mdb-nightly-20260102T000000.mender", strings.Repeat("b", 64))

	u.handleApplyStagedUpdates()

	if len(*installs) != 1 || (*installs)[0] != newer {
		t.Fatalf("installArtifact calls = %v, want [%s]", *installs, newer)
	}
}

// TestHandleApplyStagedUpdatesNothingStagedPublishesNoop: a push that finds
// nothing newer than the running version must publish staged-noop, not a refusal.
func TestHandleApplyStagedUpdatesNothingStagedPublishesNoop(t *testing.T) {
	u, mr, installs := newStagedTestUpdater(t, "mdb")
	dir := u.mender.GetDownloadDir()
	mr.HSet("version:mdb", "version_id", "nightly-20260101T000000")

	writeBaseMender(t, dir, "librescoot-unu-mdb-nightly-20260101T000000.mender", strings.Repeat("a", 64))

	u.handleApplyStagedUpdates()

	if got := mr.HGet("ota", "status:mdb"); got != "staged-noop" {
		t.Errorf("status:mdb = %q, want staged-noop", got)
	}
	if got := mr.HGet("ota", "error:mdb"); got != "" {
		t.Errorf("error:mdb = %q, want empty", got)
	}
	if len(*installs) != 0 {
		t.Fatalf("install ran with nothing staged: %v", *installs)
	}
}

// TestHandleApplyStagedUpdatesRefusesForkedChain pins that the resolver's
// fork refusal reaches the handler as staged-updates-refused, with nothing
// installed. It must be driven through the handler: removing the
// ResolveStagedDeltaChain call from handleApplyStagedUpdates would otherwise
// leave the suite green.
func TestHandleApplyStagedUpdatesRefusesForkedChain(t *testing.T) {
	u, mr, installs := newStagedTestUpdater(t, "mdb")
	dir := u.mender.GetDownloadDir()
	mr.HSet("version:mdb", "version_id", "nightly-20260101T000000")

	baseSum := strings.Repeat("a", 64)
	writeBaseMender(t, dir, "librescoot-unu-mdb-nightly-20260101T000000.mender", baseSum)
	// Both deltas were built from the base image: a fork.
	writeDeltaFile(t, dir, "librescoot-unu-mdb-nightly-20260102T000000.delta", baseSum, strings.Repeat("b", 64))
	writeDeltaFile(t, dir, "librescoot-unu-mdb-nightly-20260103T000000.delta", baseSum, strings.Repeat("c", 64))

	u.handleApplyStagedUpdates()

	waitForField(t, mr, "ota", "error:mdb", "staged-updates-refused")
	if len(*installs) != 0 {
		t.Fatalf("install ran for a forked staged set: %v", *installs)
	}
}

// TestHandleApplyStagedUpdatesRefusalSuppressedOnShutdown pins the upstream
// shutdown guard at the staged refusal site: a canceled context means the board
// is rebooting or the service is stopping, so a refused staged set must not
// publish a terminal error (the next instance restores the lifecycle).
func TestHandleApplyStagedUpdatesRefusalSuppressedOnShutdown(t *testing.T) {
	u, mr, installs := newStagedTestUpdater(t, "mdb")
	dir := u.mender.GetDownloadDir()
	mr.HSet("version:mdb", "version_id", "nightly-20260101T000000")

	baseSum := strings.Repeat("a", 64)
	writeBaseMender(t, dir, "librescoot-unu-mdb-nightly-20260101T000000.mender", baseSum)
	// A fork that would normally be refused with a terminal
	// staged-updates-refused error.
	writeDeltaFile(t, dir, "librescoot-unu-mdb-nightly-20260102T000000.delta", baseSum, strings.Repeat("b", 64))
	writeDeltaFile(t, dir, "librescoot-unu-mdb-nightly-20260103T000000.delta", baseSum, strings.Repeat("c", 64))

	u.cancel()
	u.handleApplyStagedUpdates()

	if got := mr.HGet("ota", "error:mdb"); got != "" {
		t.Errorf("shutdown refusal published error:mdb = %q, want empty", got)
	}
	if len(*installs) != 0 {
		t.Fatalf("install ran for a forked staged set: %v", *installs)
	}
}

// TestHandleApplyStagedUpdatesAppliesMultiDeltaChain drives a resolvable chain
// end to end through the handler: plan -> resolver order -> apply -> install
// tail. The applier is stubbed (a real one needs xdelta3 and a real base), so
// the seam records the ordered chain the handler hands it.
func TestHandleApplyStagedUpdatesAppliesMultiDeltaChain(t *testing.T) {
	u, mr, installs := newStagedTestUpdater(t, "mdb")
	dir := u.mender.GetDownloadDir()
	mr.HSet("version:mdb", "version_id", "nightly-20260101T000000")

	baseSum := strings.Repeat("a", 64)
	writeBaseMender(t, dir, "librescoot-unu-mdb-nightly-20260101T000000.mender", baseSum)
	d1 := writeDeltaFile(t, dir, "librescoot-unu-mdb-nightly-20260102T000000.delta", baseSum, strings.Repeat("b", 64))
	d2 := writeDeltaFile(t, dir, "librescoot-unu-mdb-nightly-20260103T000000.delta", strings.Repeat("b", 64), strings.Repeat("c", 64))

	var applied []string
	assembled := filepath.Join(dir, "assembled-from-deltas.mender")
	u.applyDeltaChain = func(_ context.Context, deltaPaths, _ []string, baseVersion string, _ mender.DeltaProgressCallback) (string, error) {
		applied = append([]string{}, deltaPaths...)
		if baseVersion != "nightly-20260101t000000" {
			t.Errorf("baseVersion = %q, want the running version's normalized base", baseVersion)
		}
		return assembled, nil
	}
	// Past the delta dry-run guard, and with the MDB reboot claimed by UMS the
	// reboot trigger returns without waiting for a vehicle state.
	u.config.DryRun = false
	mr.HSet("ota", "reboot-owner:mdb", "ums")

	u.handleApplyStagedUpdates()

	if strings.Join(applied, ",") != d1+","+d2 {
		t.Fatalf("applied chain = %v, want [%s %s]", applied, d1, d2)
	}
	if len(*installs) != 1 || (*installs)[0] != assembled {
		t.Fatalf("installArtifact calls = %v, want [%s]", *installs, assembled)
	}
}

// TestHandleApplyStagedUpdatesHoldsInstallNotPreparing observes the power-hold
// handover at the instant the install runs: the install hold is present while
// the preparing hold is already gone, so there is no window with neither.
func TestHandleApplyStagedUpdatesHoldsInstallNotPreparing(t *testing.T) {
	u, mr, installs := newStagedTestUpdater(t, "mdb")
	dir := u.mender.GetDownloadDir()
	mr.HSet("version:mdb", "version_id", "nightly-20260101T000000")

	baseSum := strings.Repeat("a", 64)
	writeBaseMender(t, dir, "librescoot-unu-mdb-nightly-20260101T000000.mender", baseSum)
	writeDeltaFile(t, dir, "librescoot-unu-mdb-nightly-20260102T000000.delta", baseSum, strings.Repeat("b", 64))

	var installHold, preparingHold bool
	u.installArtifact = func(path string, progress mender.InstallProgressCallback) error {
		*installs = append(*installs, path)
		installHold = mr.HGet(inhibitor.InhibitHashKey, "install:mdb") != ""
		preparingHold = mr.HGet(inhibitor.InhibitHashKey, "preparing:mdb") != ""
		return nil
	}
	// The real applier needs xdelta3 and a real base image; only the install
	// tail's power holds are under test here.
	assembled := filepath.Join(dir, "assembled-from-deltas.mender")
	u.applyDeltaChain = func(_ context.Context, _, _ []string, _ string, _ mender.DeltaProgressCallback) (string, error) {
		return assembled, nil
	}
	u.config.DryRun = false
	mr.HSet("ota", "reboot-owner:mdb", "ums")

	u.handleApplyStagedUpdates()

	if len(*installs) == 0 {
		t.Fatal("install did not run")
	}
	if !installHold {
		t.Error("install hold is absent at install time")
	}
	if preparingHold {
		t.Error("preparing hold is still present at install time")
	}
}

// TestInstallAssembledAndRebootAbortsWithoutInstallingStatus pins the
// requireInstallingStatus abort: when the installing status cannot be
// published, the full-image path must not write an image it cannot commit.
func TestInstallAssembledAndRebootAbortsWithoutInstallingStatus(t *testing.T) {
	u, mr, installs := newStagedTestUpdater(t, "mdb")
	mr.SetError("redis unavailable")

	u.installAssembledAndReboot("image.mender", nil, nil, true, "")

	if len(*installs) != 0 {
		t.Fatalf("install ran without an installing status: %v", *installs)
	}
}

// TestHandleApplyStagedUpdatesRefusesMenderAndDelta pins the conflict rule on
// the command path: a newer full image and a delta together install nothing.
func TestHandleApplyStagedUpdatesRefusesMenderAndDelta(t *testing.T) {
	u, mr, installs := newStagedTestUpdater(t, "mdb")
	dir := u.mender.GetDownloadDir()
	mr.HSet("version:mdb", "version_id", "nightly-20260101T000000")

	writeBaseMender(t, dir, "librescoot-unu-mdb-nightly-20260101T000000.mender", strings.Repeat("a", 64))
	writeBaseMender(t, dir, "librescoot-unu-mdb-nightly-20260102T000000.mender", strings.Repeat("b", 64))
	writeDeltaFile(t, dir, "librescoot-unu-mdb-nightly-20260103T000000.delta", strings.Repeat("b", 64), strings.Repeat("c", 64))

	u.handleApplyStagedUpdates()

	waitForField(t, mr, "ota", "error:mdb", "staged-updates-refused")
	if len(*installs) != 0 {
		t.Fatalf("install ran for a refused staged set: %v", *installs)
	}
}

// TestHandleUpdateFromFileFullImageStillInstalls pins the single-file path:
// update-from-file with one .mender must still go all the way to the install
// handoff, unchanged by the staged-updates refactor.
func TestHandleUpdateFromFileFullImageStillInstalls(t *testing.T) {
	u, mr, installs := newStagedTestUpdater(t, "mdb")
	dir := u.mender.GetDownloadDir()
	mr.HSet("version:mdb", "version_id", "nightly-20260101t000000")

	src := writeBaseMender(t, dir, "librescoot-unu-mdb-nightly-20260102T000000.mender", strings.Repeat("a", 64))

	u.handleUpdateFromFile(src)

	if len(*installs) != 1 || (*installs)[0] != src {
		t.Fatalf("installArtifact calls = %v, want [%s]", *installs, src)
	}
}

// TestHandleUpdateFromFileSingleDeltaUnchanged pins the other half of the
// single-file path: one .delta still reports the same no-base-image status it
// did before the staged-updates handler existed.
func TestHandleUpdateFromFileSingleDeltaUnchanged(t *testing.T) {
	u, mr, installs := newStagedTestUpdater(t, "mdb")
	dir := u.mender.GetDownloadDir()
	mr.HSet("version:mdb", "version_id", "nightly-20260101T000000")

	src := writeDeltaFile(t, dir, "librescoot-unu-mdb-nightly-20260102T000000.delta", strings.Repeat("a", 64), strings.Repeat("b", 64))

	u.handleUpdateFromFile(src)

	waitForField(t, mr, "ota", "error:mdb", "no-base-image")
	if len(*installs) != 0 {
		t.Fatalf("install ran without a base image: %v", *installs)
	}
}
