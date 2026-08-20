package boot

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// OCOTPPath is the nvmem node exposing the i.MX6 OCOTP fuse bank.
const OCOTPPath = "/sys/bus/nvmem/devices/imx-ocotp0/nvmem"

// BootTarget is where the boot ROM reads the bootloader from.
type BootTarget int

const (
	// BootTargetUserArea means the ROM reads from the eMMC user area, so the
	// bootloader lives on /dev/mmcblkN itself.
	BootTargetUserArea BootTarget = iota
	// BootTargetBootPartition1 means the ROM reads from the first eMMC boot
	// partition, /dev/mmcblkNboot0.
	BootTargetBootPartition1
)

func (t BootTarget) String() string {
	switch t {
	case BootTargetUserArea:
		return "user area"
	case BootTargetBootPartition1:
		return "boot partition 1"
	}
	return "unknown"
}

// Device returns the block device for this target, given the base eMMC device
// (e.g. "/dev/mmcblk3").
func (t BootTarget) Device(base string) string {
	if t == BootTargetBootPartition1 {
		return base + "boot0"
	}
	return base
}

// bootCfg2WordOffset is the byte offset of OCOTP bank 0 word 5 in the nvmem
// node. BOOT_CFG2 occupies bits [15:8] of that word.
const bootCfg2WordOffset = 5 * 4

// BootTargetFromFuses decodes which eMMC region the boot ROM reads from, out of
// the OCOTP fuse bank.
//
// This is the only authority on the question. The filesystem layout is not:
// both boards have a boot partition and both have a user area, and which one is
// live is a fuse decision taken at manufacture. Deriving the write target from
// anything else is how DBC bootloader updates ended up being written to a
// partition the ROM never reads (librescoot-tlv6).
//
// BOOT_CFG2 bits [4:3] select the region. Two values are known:
//
//	01  boot partition 1   (observed on the MDB, BOOT_CFG2 0x28)
//	11  user area          (observed on the DBC, BOOT_CFG2 0x38)
//
// Anything else is refused rather than guessed. A wrong guess here writes a
// bootloader to the wrong place — silently inert in the lucky direction, and a
// bricked board in the unlucky one.
func BootTargetFromFuses(r io.Reader) (BootTarget, error) {
	buf := make([]byte, bootCfg2WordOffset+4)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, fmt.Errorf("read OCOTP bank 0 word 5: %w", err)
	}
	word := binary.LittleEndian.Uint32(buf[bootCfg2WordOffset:])
	cfg2 := byte(word >> 8)
	switch (cfg2 >> 3) & 0x3 {
	case 0b01:
		return BootTargetBootPartition1, nil
	case 0b11:
		return BootTargetUserArea, nil
	default:
		return 0, fmt.Errorf("unrecognised BOOT_CFG2 0x%02x: bits[4:3] = %02b, expected 01 or 11", cfg2, (cfg2>>3)&0x3)
	}
}

// ReadBootTarget decodes the boot target from the running system's fuses.
func ReadBootTarget() (BootTarget, error) {
	f, err := os.Open(OCOTPPath)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", OCOTPPath, err)
	}
	defer f.Close()
	return BootTargetFromFuses(f)
}
