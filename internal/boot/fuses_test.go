package boot

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// ocotp builds a fake nvmem image with the given value in bank 0 word 5.
func ocotp(word5 uint32) []byte {
	buf := make([]byte, 64)
	binary.LittleEndian.PutUint32(buf[bootCfg2WordOffset:], word5)
	return buf
}

// The two values actually read off the boards. MDB word 5 is 0x00002860
// (BOOT_CFG2 0x28, bits[4:3] = 01) and DBC is 0x00003860 (0x38, 11).
func TestBootTargetFromFuses_RealBoards(t *testing.T) {
	cases := []struct {
		name  string
		word5 uint32
		want  BootTarget
		dev   string
	}{
		{"MDB", 0x00002860, BootTargetBootPartition1, "/dev/mmcblk1boot0"},
		{"DBC", 0x00003860, BootTargetUserArea, "/dev/mmcblk3"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := BootTargetFromFuses(bytes.NewReader(ocotp(c.word5)))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("target = %v, want %v", got, c.want)
			}
			base := map[string]string{"MDB": "/dev/mmcblk1", "DBC": "/dev/mmcblk3"}[c.name]
			if dev := got.Device(base); dev != c.dev {
				t.Errorf("Device(%s) = %s, want %s", base, dev, c.dev)
			}
		})
	}
}

// The point of the whole exercise: an unreadable or unrecognised fuse must
// refuse rather than fall back to a guess. Guessing is what wrote DBC
// bootloaders to a partition the ROM never reads.
func TestBootTargetFromFuses_RefusesToGuess(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
	}{
		{"bits 00", ocotp(0x00002060)},    // BOOT_CFG2 0x20, bits[4:3] = 00
		{"bits 10", ocotp(0x00003060)},    // BOOT_CFG2 0x30, bits[4:3] = 10
		{"all zeroes", ocotp(0x00000000)}, // unprogrammed / wrong node
		{"short read", make([]byte, 8)},   // nvmem smaller than word 5
		{"empty", nil},                    // node present but unreadable
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := BootTargetFromFuses(bytes.NewReader(c.in)); err == nil {
				t.Fatal("expected an error, got none — a guess here can brick a board")
			}
		})
	}
}

// Device() is what turns the decoded target into the path the updater writes
// to, so the boot0 suffix must only ever be appended for the boot-partition case.
func TestBootTarget_Device(t *testing.T) {
	if got := BootTargetUserArea.Device("/dev/mmcblk3"); got != "/dev/mmcblk3" {
		t.Errorf("user area device = %s, want the bare device", got)
	}
	if got := BootTargetBootPartition1.Device("/dev/mmcblk1"); got != "/dev/mmcblk1boot0" {
		t.Errorf("boot partition device = %s, want the boot0 suffix", got)
	}
}

func TestValidateIMX(t *testing.T) {
	good := make([]byte, 64)
	good[0], good[1], good[2], good[3] = 0xD1, 0x00, 0x20, 0x40 // tag, len 32, v0x40

	if err := validateIMX(good); err != nil {
		t.Fatalf("a real IVT header was rejected: %v", err)
	}

	v41 := append([]byte(nil), good...)
	v41[3] = 0x41
	if err := validateIMX(v41); err != nil {
		t.Errorf("header version 0x41 rejected: %v", err)
	}

	bad := []struct {
		name string
		in   []byte
		want string
	}{
		{"truncated", good[:16], "too short"},
		{"wrong tag", func() []byte { b := append([]byte(nil), good...); b[0] = 0x27; return b }(), "IVT tag"},
		{"wrong version", func() []byte { b := append([]byte(nil), good...); b[3] = 0x00; return b }(), "header version"},
		{"absurd length", func() []byte { b := append([]byte(nil), good...); b[1], b[2] = 0x00, 0x04; return b }(), "declares length"},
		{"all zeroes", make([]byte, 64), "IVT tag"},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			err := validateIMX(c.in)
			if err == nil {
				t.Fatal("expected rejection, got none — this would be written over a live bootloader")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err, c.want)
			}
		})
	}
}
