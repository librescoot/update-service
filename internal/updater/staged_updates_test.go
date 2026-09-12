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
	u := &Updater{
		config:    &config.Config{Component: component, DownloadDir: t.TempDir(), DryRun: true},
		redis:     rc,
		status:    status.NewReporter(rc.GetClient(), component, logger),
		mender:    mender.NewManager(t.TempDir(), func() mender.Budget { return mender.Budget{} }, logger),
		inhibitor: inhibitor.New(rc.GetClient(), logger),
		power:     power.New(rc.GetClient(), logger),
		installArtifact: func(path string, progress mender.InstallProgressCallback) error {
			*installs = append(*installs, path)
			return nil
		},
		logger: logger,
		ctx:    ctx,
		cancel: cancel,
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
