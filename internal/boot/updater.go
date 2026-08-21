package boot

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"syscall"
)

const LocalAssetsPath = "/usr/share/boot-assets"

// BootUpdater manages boot partition updates (zImage, DTB, U-Boot).
type BootUpdater struct {
	mountPoint  string // e.g. /uboot — retained only to locate the eMMC device
	bootDevice  string // e.g. /dev/mmcblk3boot0
	forceROPath string // e.g. /sys/block/mmcblk3boot0/force_ro
	ubootSeek   int64  // 512-byte blocks to skip before writing U-Boot (default 2)
	logger      *log.Logger
}

// New creates a BootUpdater from the given parameters.
func New(mountPoint, bootDevice string, ubootSeek int64, logger *log.Logger) *BootUpdater {
	forceROPath := ""
	if bootDevice != "" {
		// /dev/mmcblk3boot0 → /sys/block/mmcblk3boot0/force_ro
		dev := strings.TrimPrefix(bootDevice, "/dev/")
		forceROPath = "/sys/block/" + dev + "/force_ro"
	}
	return &BootUpdater{
		mountPoint:  mountPoint,
		bootDevice:  bootDevice,
		forceROPath: forceROPath,
		ubootSeek:   ubootSeek,
		logger:      logger,
	}
}

// DetectBootDevice reads /proc/mounts, finds the device mounted at mountPoint,
// strips the trailing partition number (p1), and appends "boot0".
// E.g.: /dev/mmcblk3p1 → /dev/mmcblk3boot0
func DetectBootDevice(mountPoint string) (string, error) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return "", fmt.Errorf("open /proc/mounts: %w", err)
	}
	defer f.Close()
	return detectFromReader(f, mountPoint)
}

// detectFromReader is the testable core of DetectBootDevice.
func detectFromReader(r io.Reader, mountPoint string) (string, error) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		device, mnt := fields[0], fields[1]
		if mnt != mountPoint {
			continue
		}
		// Strip partition suffix: /dev/mmcblk3p1 → /dev/mmcblk3
		base := device
		if idx := strings.LastIndex(base, "p"); idx >= 0 {
			candidate := base[:idx]
			// Make sure what we stripped is purely digits
			suffix := base[idx+1:]
			allDigits := len(suffix) > 0
			for _, ch := range suffix {
				if ch < '0' || ch > '9' {
					allDigits = false
					break
				}
			}
			if allDigits {
				base = candidate
			}
		}
		return base + "boot0", nil
	}

	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("reading mounts: %w", err)
	}
	return "", fmt.Errorf("no device found mounted at %s", mountPoint)
}

// UBootPath is the U-Boot image inside a boot-asset bundle.
const UBootPath = "u-boot-dtb.imx"

// HasLocalAssets reports whether a boot-asset bundle is baked into this rootfs.
func HasLocalAssets() bool {
	_, err := os.Stat(LocalAssetsPath + "/" + UBootPath)
	return err == nil
}

// UpToDate reports whether the boot region already holds the U-Boot image in
// assetDir, so there is nothing to do.
//
// This replaces the boot-version file. That file recorded a hash over the whole
// bundle — kernel, dtb and U-Boot — yet gated a write of U-Boot alone, so a
// kernel-only change triggered a pointless rewrite of the boot region. It was
// also bookkeeping that could disagree with the hardware, and did: it recorded
// success for writes that never reached anything the board reads.
//
// The device is its own source of truth, so there is no file left to drift.
func (b *BootUpdater) UpToDate(assetDir string) (bool, error) {
	imxData, err := os.ReadFile(assetDir + "/" + UBootPath)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", assetDir+"/"+UBootPath, err)
	}
	return b.ubootMatches(imxData)
}

// Apply writes the U-Boot image from a boot-asset bundle to the boot region.
//
// U-Boot only. The kernel and dtb in the bundle are deliberately not written:
// U-Boot loads both from /boot inside the rootfs — confirmed on both boards,
// bootcmd resolves ${mender_uboot_root} to the rootfs partition — and the
// mender rootfs artifact already delivers them there. Writing them to the FAT
// at mountPoint produced byte-identical copies that nothing ever read.
func (b *BootUpdater) Apply(ctx context.Context, extractDir string) error {
	imxPath := extractDir + "/" + UBootPath
	b.logger.Printf("[boot] writing U-Boot: %s → %s", imxPath, b.bootDevice)
	if err := b.writeUBoot(imxPath); err != nil {
		return fmt.Errorf("write U-Boot: %w", err)
	}
	syscall.Sync()
	return nil
}

// ubootMatches reports whether the target region already holds exactly want.
// Read-only: force_ro does not need unlocking to read, so this cannot itself
// put the boot region at risk.
func (b *BootUpdater) ubootMatches(want []byte) (bool, error) {
	f, err := os.Open(b.bootDevice)
	if err != nil {
		return false, fmt.Errorf("open %s: %w", b.bootDevice, err)
	}
	defer f.Close()

	offset := b.ubootSeek * 512
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return false, fmt.Errorf("seek %s to %d: %w", b.bootDevice, offset, err)
	}
	existing := make([]byte, len(want))
	if _, err := io.ReadFull(f, existing); err != nil {
		return false, fmt.Errorf("read %s: %w", b.bootDevice, err)
	}
	return sha256sum(existing) == sha256sum(want), nil
}

// writeUBoot validates the image, skips the write when the target already
// matches, then unlocks force_ro, seeks to ubootSeek*512 bytes, writes imx
// data, reads back and verifies sha256, and re-locks force_ro.
func (b *BootUpdater) writeUBoot(imxPath string) error {
	imxData, err := os.ReadFile(imxPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", imxPath, err)
	}
	if err := validateIMX(imxData); err != nil {
		return fmt.Errorf("refusing to write %s: %w", imxPath, err)
	}
	expectedHash := sha256sum(imxData)

	// Nothing to do if the target already holds exactly this image. Writing
	// over a live bootloader is the one operation here with no cheap recovery,
	// so the normal case — an update that does not change U-Boot — should not
	// touch the region at all.
	if same, err := b.ubootMatches(imxData); err != nil {
		b.logger.Printf("[boot] could not compare existing U-Boot, writing anyway: %v", err)
	} else if same {
		b.logger.Printf("[boot] U-Boot already at %s, not rewriting", expectedHash)
		return nil
	}

	// Unlock
	if err := os.WriteFile(b.forceROPath, []byte("0\n"), 0200); err != nil {
		return fmt.Errorf("unlock %s: %w", b.forceROPath, err)
	}
	defer func() {
		if err := os.WriteFile(b.forceROPath, []byte("1\n"), 0200); err != nil {
			b.logger.Printf("[boot] warning: failed to re-lock %s: %v", b.forceROPath, err)
		}
	}()

	f, err := os.OpenFile(b.bootDevice, os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("open %s: %w", b.bootDevice, err)
	}
	defer f.Close()

	offset := b.ubootSeek * 512
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return fmt.Errorf("seek %s to %d: %w", b.bootDevice, offset, err)
	}
	if _, err := f.Write(imxData); err != nil {
		return fmt.Errorf("write %s: %w", b.bootDevice, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", b.bootDevice, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", b.bootDevice, err)
	}

	// Read back and verify
	rf, err := os.Open(b.bootDevice)
	if err != nil {
		return fmt.Errorf("open for verify %s: %w", b.bootDevice, err)
	}
	defer rf.Close()

	if _, err := rf.Seek(offset, io.SeekStart); err != nil {
		return fmt.Errorf("seek for verify %s: %w", b.bootDevice, err)
	}
	readBack := make([]byte, len(imxData))
	if _, err := io.ReadFull(rf, readBack); err != nil {
		return fmt.Errorf("read back %s: %w", b.bootDevice, err)
	}
	actualHash := sha256sum(readBack)
	if actualHash != expectedHash {
		return fmt.Errorf("verify U-Boot: sha256 mismatch (expected %s, got %s)", expectedHash, actualHash)
	}

	b.logger.Printf("[boot] U-Boot written and verified (%d bytes at offset %d)", len(imxData), offset)
	return nil
}

// validateIMX sanity-checks that data really is an i.MX bootable image before
// it is written over a live bootloader.
//
// The readback verify below proves the bytes landed; it cannot tell whether
// they were the right bytes. Without this a truncated or wrong-architecture
// artifact writes cleanly, verifies cleanly, and bricks the board on the next
// power cycle.
//
// The i.MX Image Vector Table starts the file: tag 0xD1, a big-endian length,
// then the header version. This is the same signature used to establish which
// eMMC region the DBC actually boots from (bytes d1 00 20 40 at offset 1024).
func validateIMX(data []byte) error {
	const ivtHeaderLen = 32
	if len(data) < ivtHeaderLen {
		return fmt.Errorf("too short for an IVT header: %d bytes", len(data))
	}
	if data[0] != 0xD1 {
		return fmt.Errorf("not an i.MX image: IVT tag is 0x%02x, expected 0xd1", data[0])
	}
	if v := data[3]; v != 0x40 && v != 0x41 {
		return fmt.Errorf("unexpected IVT header version 0x%02x, expected 0x40 or 0x41", v)
	}
	if l := int(data[1])<<8 | int(data[2]); l < ivtHeaderLen {
		return fmt.Errorf("IVT declares length %d, expected at least %d", l, ivtHeaderLen)
	}
	return nil
}

func sha256sum(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
