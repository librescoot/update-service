package boot

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// Synthetic imximage v2: SD prefix 0x400, initial load 0x1000, entry
// at source 0xc00. tools/imximage.c set_imx_hdr_v2 and imximage_set_header
// use BootData.start + rounded size for CSF, not self + rounded size.
func representativeIMX(csf bool) []byte {
	data := make([]byte, 0x2c00)
	copy(data, []byte{0xd1, 0, 32, 0x40})
	put := func(off int, v uint32) { binary.LittleEndian.PutUint32(data[off:], v) }
	const self = 0x177ff400
	put(4, self+0xc00)
	put(12, self+44)
	put(16, self+32)
	put(20, self)
	put(32, self-1024)
	put(36, 0x3000)
	copy(data[44:], []byte{0xd2, 0, 16, 0x40, 0xcc, 0, 12, 4, 0x02, 0x0e, 0, 0, 0, 0, 0, 1})
	for i := 0xc00; i < len(data); i++ {
		data[i] = byte(i%251 + 1)
	}
	if csf {
		put(24, self+0x2c00)
		put(36, 0x5000)
	}
	return data
}

func TestIMXValidation(t *testing.T) {
	for _, csf := range []bool{false, true} {
		if err := validateIMX(representativeIMX(csf)); err != nil {
			t.Fatal(err)
		}
	}
	// Non-padded file with legitimate rounding gap before a reserved CSF.
	if err := validateIMX(representativeIMX(true)[:0x2b21]); err != nil {
		t.Fatal(err)
	}
	tests := map[string]func([]byte) []byte{
		"empty":            func(d []byte) []byte { return nil },
		"signature only":   func(d []byte) []byte { return d[:32] },
		"truncated":        func(d []byte) []byte { return d[:0x1c00] },
		"truncated entry":  func(d []byte) []byte { return d[:0xc10] },
		"length31":         func(d []byte) []byte { d[2] = 31; return d },
		"length33":         func(d []byte) []byte { d[2] = 33; return d },
		"version":          func(d []byte) []byte { d[3] = 0x42; return d },
		"tag":              func(d []byte) []byte { d[0] = 0; return d },
		"self":             func(d []byte) []byte { clear(d[20:24]); return d },
		"boot before self": func(d []byte) []byte { binary.LittleEndian.PutUint32(d[16:], 1); return d },
		"boot outside":     func(d []byte) []byte { binary.LittleEndian.PutUint32(d[16:], math.MaxUint32); return d },
		"entry header":     func(d []byte) []byte { copy(d[4:8], d[20:24]); return d },
		"entry outside":    func(d []byte) []byte { binary.LittleEndian.PutUint32(d[4:], math.MaxUint32); return d },
		"dcd length":       func(d []byte) []byte { d[45] = 0xff; return d },
		"dcd outside":      func(d []byte) []byte { binary.LittleEndian.PutUint32(d[12:], math.MaxUint32); return d },
		"start":            func(d []byte) []byte { clear(d[32:36]); return d },
		"size overflow":    func(d []byte) []byte { binary.LittleEndian.PutUint32(d[36:], math.MaxUint32); return d },
		"size short":       func(d []byte) []byte { binary.LittleEndian.PutUint32(d[36:], 1024); return d },
		"plugin":           func(d []byte) []byte { d[40] = 1; return d },
		"blank payload":    func(d []byte) []byte { clear(d[0xc00:]); return d },
		"erased payload": func(d []byte) []byte {
			for i := 0xc00; i < len(d); i++ {
				d[i] = 0xff
			}
			return d
		},
		"CSF outside":    func(d []byte) []byte { binary.LittleEndian.PutUint32(d[24:], math.MaxUint32); return d },
		"CSF misaligned": func(d []byte) []byte { binary.LittleEndian.PutUint32(d[24:], 0x17802001); return d },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			data := mutate(representativeIMX(false))
			if validateIMX(data) == nil {
				t.Fatal("accepted malformed image")
			}
			b, dir, state := testUpdater(t, data)
			if _, err := b.UpToDate(dir); err == nil {
				t.Fatal("UpToDate accepted invalid image")
			}
			if err := b.Apply(context.Background(), dir); err == nil {
				t.Fatal("Apply accepted invalid image")
			}
			if state.opens != 0 || len(state.locks) != 0 {
				t.Fatal("malformed image touched target")
			}
		})
	}
}

type ioState struct {
	opens, writes, syncs                                 int
	locks                                                []bool
	compareErr, verifyErr, syncErr, unlockErr, relockErr bool
	corrupt, shortWrite                                  bool
	size                                                 uint64
}
type testRegion struct {
	*os.File
	state    *ioState
	writable bool
}

func (f *testRegion) ReadAt(p []byte, off int64) (int, error) {
	if (!f.writable && f.state.compareErr) || (f.writable && f.state.verifyErr) {
		return 0, errors.New("injected read failure")
	}
	n, err := f.File.ReadAt(p, off)
	if f.writable && f.state.corrupt && n > 0 {
		p[0] ^= 1
	}
	return n, err
}
func (f *testRegion) WriteAt(p []byte, off int64) (int, error) {
	f.state.writes++
	if f.state.shortWrite {
		return len(p) - 1, nil
	}
	return f.File.WriteAt(p, off)
}
func (f *testRegion) Sync() error {
	f.state.syncs++
	if f.state.syncErr {
		return errors.New("injected sync failure")
	}
	return f.File.Sync()
}
func testUpdater(t *testing.T, data []byte) (*BootUpdater, string, *ioState) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, UBootPath), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, manifestName), []byte(sha256sum(data)+"  "+UBootPath+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, make([]byte, 65536), 0600); err != nil {
		t.Fatal(err)
	}
	state := &ioState{size: 65536}
	b := New("/uboot", "/dev/mmcblk3boot0", 2, log.New(io.Discard, "", 0))
	b.io = bootIO{
		open: func(_ string, flags int) (regionFile, error) {
			state.opens++
			f, err := os.OpenFile(target, flags, 0)
			if err != nil {
				return nil, err
			}
			return &testRegion{f, state, flags != os.O_RDONLY}, nil
		},
		inspect: func(regionFile, string) (uint64, error) { return state.size, nil },
		setReadOnly: func(_ string, ro bool) error {
			state.locks = append(state.locks, ro)
			if (ro && state.relockErr) || (!ro && state.unlockErr) {
				return errors.New("injected force_ro failure")
			}
			return nil
		},
	}
	return b, dir, state
}

func TestBootCancellationBeforeWrite(t *testing.T) {
	for _, at := range []string{"compare", "unlock"} {
		t.Run(at, func(t *testing.T) {
			b, dir, s := testUpdater(t, representativeIMX(false))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if at == "compare" {
				inspect := b.io.inspect
				b.io.inspect = func(f regionFile, path string) (uint64, error) {
					cancel()
					return inspect(f, path)
				}
			} else {
				setRO := b.io.setReadOnly
				b.io.setReadOnly = func(path string, ro bool) error {
					if !ro {
						cancel()
					}
					return setRO(path, ro)
				}
			}
			if err := b.Apply(ctx, dir); !errors.Is(err, context.Canceled) {
				t.Fatalf("Apply = %v, want cancellation", err)
			}
			if s.writes != 0 {
				t.Fatal("wrote after cancellation")
			}
			if at == "compare" && len(s.locks) != 0 {
				t.Fatal("unlocked after cancellation")
			}
			if at == "unlock" && (len(s.locks) != 2 || !s.locks[1]) {
				t.Fatal("did not re-lock after cancellation")
			}
		})
	}
}

func TestBootWriteAndNoop(t *testing.T) {
	data := representativeIMX(true)
	b, dir, s := testUpdater(t, data)
	if err := b.Apply(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if s.writes != 1 || s.syncs != 1 || len(s.locks) != 2 || s.locks[0] || !s.locks[1] {
		t.Fatalf("unexpected I/O: %+v", s)
	}
	got, err := os.ReadFile(filepath.Join(dir, "target"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got[:1024], make([]byte, 1024)) || !bytes.Equal(got[1024:1024+len(data)], data) {
		t.Fatal("wrong target offset")
	}
	if same, err := b.UpToDate(dir); err != nil || !same {
		t.Fatalf("UpToDate: %v %v", same, err)
	}
	if err := b.Apply(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if s.writes != 1 || len(s.locks) != 2 {
		t.Fatal("no-op changed target")
	}
}

func TestBootIOFailures(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(*ioState)
		writes int
		want   string
	}{
		{"compare", func(s *ioState) { s.compareErr = true }, 0, "compare"},
		{"oversized", func(s *ioState) { s.size = 4096 }, 0, "exceeds"},
		{"declared CSF extent", func(s *ioState) { s.size = 0x3000 }, 0, "exceeds"},
		{"unlock", func(s *ioState) { s.unlockErr = true }, 0, "unlock"},
		{"relock", func(s *ioState) { s.relockErr = true }, 1, "re-lock"},
		{"sync", func(s *ioState) { s.syncErr = true }, 1, "sync"},
		{"readback", func(s *ioState) { s.verifyErr = true }, 1, "read back"},
		{"mismatch", func(s *ioState) { s.corrupt = true }, 1, "mismatch"},
		{"short write", func(s *ioState) { s.shortWrite = true }, 1, "short write"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, dir, s := testUpdater(t, representativeIMX(true))
			tt.setup(s)
			err := b.Apply(context.Background(), dir)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v", err)
			}
			if s.writes != tt.writes {
				t.Fatalf("writes=%d", s.writes)
			}
			if tt.name == "compare" || strings.Contains(tt.name, "extent") || tt.name == "oversized" {
				if len(s.locks) != 0 {
					t.Fatal("unlocked invalid target")
				}
			} else if len(s.locks) != 2 || !s.locks[1] {
				t.Fatal("did not re-lock")
			}
		})
	}
}

func TestBLKGETSIZE64RequestIsArchitectureCorrect(t *testing.T) {
	var want uintptr
	switch runtime.GOARCH {
	case "arm", "386":
		want = 0x80041272
	case "amd64", "arm64":
		want = 0x80081272
	default:
		return
	}
	if got := uintptr(unix.BLKGETSIZE64); got != want {
		t.Fatalf("BLKGETSIZE64 request on %s = %#x, want %#x", runtime.GOARCH, got, want)
	}
}

func TestBootTargetRejections(t *testing.T) {
	for _, seek := range []int64{-1, math.MaxInt64, math.MaxInt64 / 512, 0, 3} {
		b, dir, s := testUpdater(t, representativeIMX(false))
		b.ubootSeek = seek
		if err := b.Apply(context.Background(), dir); err == nil {
			t.Fatalf("accepted seek %d", seek)
		}
		if s.opens != 0 {
			t.Fatal("opened target for invalid seek")
		}
	}
	for _, path := range []string{"/dev/mmcblk3", "/dev/mmcblk3p1", "/dev/mmcblk3boot1", "/tmp/target", "/dev/../dev/mmcblk3boot0"} {
		b, dir, s := testUpdater(t, representativeIMX(false))
		b.bootDevice = path
		if err := b.Apply(context.Background(), dir); err == nil {
			t.Fatalf("accepted %s", path)
		}
		if s.opens != 0 {
			t.Fatal("opened unsupported target")
		}
		if New("", path, 2, nil).forceROPath != "" {
			t.Fatal("invented force_ro path")
		}
	}
	f, err := os.CreateTemp(t.TempDir(), "regular")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := inspectRegion(f, "/dev/mmcblk3boot0"); err == nil {
		t.Fatal("accepted regular file")
	}
	for _, tt := range []struct{ dev, size string }{{"179:9", "10"}, {"179:8", "0"}, {"179:8", "-1"}, {"179:8", "18446744073709551615"}, {"179:8", "abc"}} {
		if _, err := validateRegionMetadata(179<<8|8, tt.dev, tt.size); err == nil {
			t.Fatalf("accepted %+v", tt)
		}
	}
	if size, err := validateRegionMetadata(179<<8|8, "179:8\n", "8192\n"); err != nil || size != 4194304 {
		t.Fatalf("size=%d err=%v", size, err)
	}
}

func TestSourceAndTargetReadFailures(t *testing.T) {
	for _, kind := range []string{"missing", "source directory", "short target", "inspect", "write descriptor inspect", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			b, dir, s := testUpdater(t, representativeIMX(false))
			ctx := context.Background()
			switch kind {
			case "missing", "source directory":
				if err := os.Remove(filepath.Join(dir, UBootPath)); err != nil {
					t.Fatal(err)
				}
				if kind == "source directory" {
					if err := os.Mkdir(filepath.Join(dir, UBootPath), 0700); err != nil {
						t.Fatal(err)
					}
				}
			case "short target":
				if err := os.Truncate(filepath.Join(dir, "target"), 1024); err != nil {
					t.Fatal(err)
				}
			case "inspect", "write descriptor inspect":
				b.io.inspect = func(f regionFile, _ string) (uint64, error) {
					if kind == "inspect" || f.(*testRegion).writable {
						return 0, errors.New("identity failure")
					}
					return s.size, nil
				}
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if err := b.Apply(ctx, dir); err == nil {
				t.Fatal("expected error")
			}
			if s.writes != 0 {
				t.Fatal("wrote despite error")
			}
			if kind == "write descriptor inspect" {
				if len(s.locks) != 2 || !s.locks[1] {
					t.Fatal("did not re-lock")
				}
			} else if len(s.locks) != 0 {
				t.Fatal("unlocked despite read failure")
			}
		})
	}
}

func TestPresentCSF(t *testing.T) {
	data := append(representativeIMX(true), make([]byte, 8192)...)
	copy(data[0x2c00:], []byte{0xd4, 0, 4, 0x40})
	if err := validateIMX(data); err != nil {
		t.Fatal(err)
	}
	data[0x2c01] = 0xff
	if err := validateIMX(data); err == nil {
		t.Fatal("accepted oversized CSF")
	}
}

func FuzzValidateIMX(f *testing.F) {
	f.Add(representativeIMX(false))
	f.Add(representativeIMX(true))
	f.Add([]byte{0xd1, 0, 32, 0x40})
	f.Fuzz(func(t *testing.T, data []byte) { _ = validateIMX(data) })
}
