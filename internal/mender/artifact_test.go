package mender

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTar writes a tar of the given name/content pairs.
func writeTar(t *testing.T, w io.Writer, entries [][2]string) {
	t.Helper()
	tw := tar.NewWriter(w)
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: e[0], Mode: 0o600, Size: int64(len(e[1]))}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, e[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
}

// gzTar returns a gzipped tar of the given entries.
func gzTar(t *testing.T, entries [][2]string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	writeTar(t, gz, entries)
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// buildArtifact writes a version 3 Mender artifact holding one rootfs payload
// of the given declared size, and returns its path.
func buildArtifact(t *testing.T, deviceTypes []string, payloadName string, payloadSize int64) string {
	t.Helper()

	hi := headerInfo{}
	hi.ArtifactDepends.DeviceType = deviceTypes
	hiJSON, err := json.Marshal(hi)
	if err != nil {
		t.Fatal(err)
	}

	header := gzTar(t, [][2]string{
		{"header-info", string(hiJSON)},
		{"headers/0000/type-info", "{}"},
	})
	payload := gzTar(t, [][2]string{
		{payloadName, strings.Repeat("\x00", int(payloadSize))},
	})

	path := filepath.Join(t.TempDir(), "test.mender")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	writeTar(t, f, [][2]string{
		{"version", `{"format":"mender","version":3}`},
		{"manifest", "deadbeef  data/0000/" + payloadName + "\n"},
		{"header.tar.gz", header},
		{"data/0000.tar.gz", payload},
	})
	return path
}

func TestReadArtifactInfo(t *testing.T) {
	path := buildArtifact(t, []string{"unu-mdb"}, "librescoot-mdb-image-unu-mdb.ext4", 4096)

	info, err := ReadArtifactInfo(path)
	if err != nil {
		t.Fatalf("ReadArtifactInfo: %v", err)
	}
	if got, want := info.PayloadSize, int64(4096); got != want {
		t.Errorf("PayloadSize = %d, want %d", got, want)
	}
	if got, want := info.PayloadName, "librescoot-mdb-image-unu-mdb.ext4"; got != want {
		t.Errorf("PayloadName = %q, want %q", got, want)
	}
	if got, want := strings.Join(info.DeviceTypes, ","), "unu-mdb"; got != want {
		t.Errorf("DeviceTypes = %q, want %q", got, want)
	}
}

func TestReadArtifactInfoRejectsNonArtifacts(t *testing.T) {
	dir := t.TempDir()

	notATar := filepath.Join(dir, "junk.mender")
	if err := os.WriteFile(notATar, []byte("this is not a tar"), 0o600); err != nil {
		t.Fatal(err)
	}

	noPayload := filepath.Join(dir, "headeronly.mender")
	f, err := os.Create(noPayload)
	if err != nil {
		t.Fatal(err)
	}
	writeTar(t, f, [][2]string{
		{"version", `{"format":"mender","version":3}`},
		{"header.tar.gz", gzTar(t, [][2]string{{"header-info", `{"artifact_depends":{"device_type":["unu-mdb"]}}`}})},
	})
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{notATar, noPayload, filepath.Join(dir, "absent.mender")} {
		if _, err := ReadArtifactInfo(path); err == nil {
			t.Errorf("ReadArtifactInfo(%s) succeeded, want error", filepath.Base(path))
		}
	}
}

// writeMenderConf writes a mender.conf naming the two rootfs devices.
func writeMenderConf(t *testing.T, dir, partA, partB string) string {
	t.Helper()
	path := filepath.Join(dir, "mender.conf")
	body := `{"RootfsPartA":"` + partA + `","RootfsPartB":"` + partB + `"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRootfsPartitions(t *testing.T) {
	dir := t.TempDir()
	base := writeMenderConf(t, dir, "/dev/mmcblk1p2", "/dev/mmcblk1p3")

	a, b, err := rootfsPartitions([]string{base})
	if err != nil {
		t.Fatalf("rootfsPartitions: %v", err)
	}
	if a != "/dev/mmcblk1p2" || b != "/dev/mmcblk1p3" {
		t.Errorf("got %q/%q, want /dev/mmcblk1p2//dev/mmcblk1p3", a, b)
	}

	// A later file wins, matching the order the rootfs-image module reads them.
	override := filepath.Join(dir, "override.conf")
	if err := os.WriteFile(override, []byte(`{"RootfsPartA":"/dev/sda1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a, b, err = rootfsPartitions([]string{base, override})
	if err != nil {
		t.Fatalf("rootfsPartitions with override: %v", err)
	}
	if a != "/dev/sda1" || b != "/dev/mmcblk1p3" {
		t.Errorf("got %q/%q, want /dev/sda1//dev/mmcblk1p3", a, b)
	}

	// Missing, unparsable and incomplete configs all fail rather than
	// returning half an answer.
	bad := filepath.Join(dir, "bad.conf")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(dir, "partial.conf")
	if err := os.WriteFile(partial, []byte(`{"RootfsPartA":"/dev/sda1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, paths := range [][]string{
		{filepath.Join(dir, "absent.conf")},
		{bad},
		{partial},
	} {
		if _, _, err := rootfsPartitions(paths); err == nil {
			t.Errorf("rootfsPartitions(%v) succeeded, want error", paths)
		}
	}
}

func TestDeviceSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slot")
	if err := os.WriteFile(path, make([]byte, 8192), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := deviceSize(path)
	if err != nil {
		t.Fatalf("deviceSize: %v", err)
	}
	if n != 8192 {
		t.Errorf("deviceSize = %d, want 8192", n)
	}

	if _, err := deviceSize(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("deviceSize on a missing path succeeded, want error")
	}
}

// fitTestInstaller returns an Installer whose fit check reads the given conf
// and reports the given sizes for the two rootfs devices.
func fitTestInstaller(t *testing.T, sizeA, sizeB int64) (*Installer, *bytes.Buffer) {
	t.Helper()
	conf := writeMenderConf(t, t.TempDir(), "/dev/testA", "/dev/testB")
	var logbuf bytes.Buffer
	return &Installer{
		logger:          log.New(&logbuf, "", 0),
		menderConfPaths: []string{conf},
		deviceSize: func(dev string) (int64, error) {
			switch dev {
			case "/dev/testA":
				return sizeA, nil
			case "/dev/testB":
				return sizeB, nil
			}
			return 0, errors.New("unknown device " + dev)
		},
	}, &logbuf
}

func TestCheckArtifactFits(t *testing.T) {
	const slot = 8192

	tests := []struct {
		name         string
		payloadSize  int64
		wantTooLarge bool
	}{
		{"well under", slot / 2, false},
		{"exactly full", slot, false},
		{"one byte over", slot + 1, true},
		{"far over", slot * 3, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inst, _ := fitTestInstaller(t, slot, slot)
			path := buildArtifact(t, []string{"unu-mdb"}, "rootfs.ext4", tc.payloadSize)

			err := inst.checkArtifactFits(path)
			if tc.wantTooLarge {
				if !errors.Is(err, ErrArtifactTooLarge) {
					t.Fatalf("payload %d into slot %d: got %v, want ErrArtifactTooLarge", tc.payloadSize, slot, err)
				}
				// The message is relayed into the ota hash and must not trip
				// the caller's corruption heuristic, which deletes the file
				// and retries on these words.
				for _, word := range []string{"gzip", "checksum", "corrupt", "truncated"} {
					if strings.Contains(err.Error(), word) {
						t.Errorf("error message contains %q, which the caller reads as corruption: %v", word, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("payload %d into slot %d: %v", tc.payloadSize, slot, err)
			}
		})
	}
}

func TestCheckArtifactFitsUsesSmallerSlot(t *testing.T) {
	// Mismatched slots must be judged against the smaller one.
	inst, _ := fitTestInstaller(t, 16384, 8192)
	path := buildArtifact(t, []string{"unu-mdb"}, "rootfs.ext4", 12288)

	if err := inst.checkArtifactFits(path); !errors.Is(err, ErrArtifactTooLarge) {
		t.Fatalf("12288 bytes against slots of 16384 and 8192: got %v, want ErrArtifactTooLarge", err)
	}
}

func TestCheckArtifactFitsSkipsWhenUndecidable(t *testing.T) {
	oversized := int64(99999)

	t.Run("unreadable mender config", func(t *testing.T) {
		var logbuf bytes.Buffer
		inst := &Installer{
			logger:          log.New(&logbuf, "", 0),
			menderConfPaths: []string{filepath.Join(t.TempDir(), "absent.conf")},
			deviceSize:      func(string) (int64, error) { return 8192, nil },
		}
		path := buildArtifact(t, []string{"unu-mdb"}, "rootfs.ext4", oversized)

		if err := inst.checkArtifactFits(path); err != nil {
			t.Fatalf("want nil so a good update is never blocked by a config problem, got %v", err)
		}
		if !strings.Contains(logbuf.String(), "Fit check skipped") {
			t.Errorf("skipping the check must be logged, got %q", logbuf.String())
		}
	})

	t.Run("unsizeable devices", func(t *testing.T) {
		var logbuf bytes.Buffer
		inst := &Installer{
			logger:          log.New(&logbuf, "", 0),
			menderConfPaths: []string{writeMenderConf(t, t.TempDir(), "/dev/testA", "/dev/testB")},
			deviceSize:      func(string) (int64, error) { return 0, errors.New("nope") },
		}
		path := buildArtifact(t, []string{"unu-mdb"}, "rootfs.ext4", oversized)

		if err := inst.checkArtifactFits(path); err != nil {
			t.Fatalf("want nil, got %v", err)
		}
		if !strings.Contains(logbuf.String(), "Fit check skipped") {
			t.Errorf("skipping the check must be logged, got %q", logbuf.String())
		}
	})

	t.Run("unreadable artifact", func(t *testing.T) {
		inst, logbuf := fitTestInstaller(t, 8192, 8192)
		junk := filepath.Join(t.TempDir(), "junk.mender")
		if err := os.WriteFile(junk, []byte("not an artifact"), 0o600); err != nil {
			t.Fatal(err)
		}

		if err := inst.checkArtifactFits(junk); err != nil {
			t.Fatalf("want nil so mender-update gets to report the real problem, got %v", err)
		}
		if !strings.Contains(logbuf.String(), "Fit check skipped") {
			t.Errorf("skipping the check must be logged, got %q", logbuf.String())
		}
	})
}

// Install must run the fit check before it execs mender-update, so an
// oversized artifact is refused without a byte reaching the passive slot.
func TestInstallRefusesOversizedArtifactBeforeExec(t *testing.T) {
	inst, _ := fitTestInstaller(t, 8192, 8192)
	path := buildArtifact(t, []string{"unu-mdb"}, "rootfs.ext4", 16384)

	err := inst.Install(path, nil)
	if !errors.Is(err, ErrArtifactTooLarge) {
		t.Fatalf("got %v, want ErrArtifactTooLarge (an exec error means the check ran too late or not at all)", err)
	}
}

// One unsizeable device is survivable: the other still bounds the write.
func TestCheckArtifactFitsWithOneDeviceUnreadable(t *testing.T) {
	var logbuf bytes.Buffer
	inst := &Installer{
		logger:          log.New(&logbuf, "", 0),
		menderConfPaths: []string{writeMenderConf(t, t.TempDir(), "/dev/testA", "/dev/testB")},
		deviceSize: func(dev string) (int64, error) {
			if dev == "/dev/testA" {
				return 0, errors.New("nope")
			}
			return 8192, nil
		},
	}

	if err := inst.checkArtifactFits(buildArtifact(t, nil, "rootfs.ext4", 9000)); !errors.Is(err, ErrArtifactTooLarge) {
		t.Fatalf("got %v, want ErrArtifactTooLarge from the one readable slot", err)
	}
	if err := inst.checkArtifactFits(buildArtifact(t, nil, "rootfs.ext4", 4000)); err != nil {
		t.Fatalf("got %v, want nil", err)
	}
}
