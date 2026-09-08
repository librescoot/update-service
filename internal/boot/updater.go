package boot

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"strings"
)

const LocalAssetsPath = "/usr/share/boot-assets"

// BootUpdater verifies and updates U-Boot in the configured boot0 region.
type BootUpdater struct {
	mountPoint  string // e.g. /uboot — retained only to locate the eMMC device
	bootDevice  string // e.g. /dev/mmcblk3boot0
	forceROPath string // e.g. /sys/block/mmcblk3boot0/force_ro
	ubootSeek   int64  // 512-byte blocks to skip before writing U-Boot (default 2)
	logger      *log.Logger
	io          bootIO
}

// New creates a BootUpdater from the given parameters.
func New(mountPoint, bootDevice string, ubootSeek int64, logger *log.Logger) *BootUpdater {
	forceROPath := ""
	if supportedBootDevice(bootDevice) {
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
		io:          systemBootIO(),
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

// UpToDate verifies the packaged image and compares it with the configured
// region. A match does not establish that the ROM boots from that region.
func (b *BootUpdater) UpToDate(assetDir string) (bool, error) {
	imxData, err := readUBootAsset(assetDir + "/" + UBootPath)
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
	if err := ctx.Err(); err != nil {
		return err
	}
	imxPath := extractDir + "/" + UBootPath
	b.logger.Printf("[boot] writing U-Boot: %s → %s", imxPath, b.bootDevice)
	if err := b.writeUBoot(ctx, imxPath); err != nil {
		return fmt.Errorf("write U-Boot: %w", err)
	}
	return nil
}

// ubootMatches reports whether the target region already holds exactly want.
// Read-only: force_ro does not need unlocking to read, so this cannot itself
// put the boot region at risk.
func (b *BootUpdater) ubootMatches(want []byte) (bool, error) {
	offset, extent, err := b.validateWrite(want)
	if err != nil {
		return false, err
	}
	f, err := b.openRegion(os.O_RDONLY, offset, extent)
	if err != nil {
		return false, err
	}
	same, err := matches(f, want, offset)
	return same, errors.Join(err, f.Close())
}

func matches(f regionFile, want []byte, offset int64) (bool, error) {
	existing := make([]byte, len(want))
	if n, err := f.ReadAt(existing, offset); err != nil {
		return false, fmt.Errorf("read boot region: %w", err)
	} else if n != len(existing) {
		return false, io.ErrUnexpectedEOF
	}
	return sha256sum(existing) == sha256sum(want), nil
}

func (b *BootUpdater) validateWrite(data []byte) (int64, uint64, error) {
	extent, err := imxExtent(data)
	if err != nil {
		return 0, 0, err
	}
	if b.ubootSeek < 0 || b.ubootSeek > math.MaxInt64/512 {
		return 0, 0, fmt.Errorf("invalid U-Boot seek %d", b.ubootSeek)
	}
	offset := b.ubootSeek * 512
	if extent > uint64(math.MaxInt64-offset) {
		return 0, 0, fmt.Errorf("U-Boot extent overflows seek")
	}
	// This updater supports the SD/eMMC layout only; source IVT stays at zero.
	if offset != 1024 {
		return 0, 0, fmt.Errorf("unsupported U-Boot offset %d (expected 1024)", offset)
	}
	if !supportedBootDevice(b.bootDevice) {
		return 0, 0, fmt.Errorf("unsupported boot region %q: only /dev/mmcblkNboot0 is supported", b.bootDevice)
	}
	return offset, extent, nil
}

func (b *BootUpdater) openRegion(flags int, offset int64, extent uint64) (regionFile, error) {
	f, err := b.io.open(b.bootDevice, flags)
	if err != nil {
		return nil, err
	}
	size, err := b.io.inspect(f, b.bootDevice)
	if err == nil && (uint64(offset) > size || extent > size-uint64(offset)) {
		err = fmt.Errorf("U-Boot extent exceeds boot region size %d", size)
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

// writeUBoot validates the image, skips the write when the target already
// matches, then unlocks force_ro, seeks to ubootSeek*512 bytes, writes imx
// data, reads back and verifies sha256, and re-locks force_ro.
func (b *BootUpdater) writeUBoot(ctx context.Context, imxPath string) (result error) {
	imxData, err := readUBootAsset(imxPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", imxPath, err)
	}
	offset, extent, err := b.validateWrite(imxData)
	if err != nil {
		return fmt.Errorf("refusing to write %s: %w", imxPath, err)
	}
	expectedHash := sha256sum(imxData)
	if err := ctx.Err(); err != nil {
		return err
	}

	// Nothing to do if the target already holds exactly this image. Writing
	// over a live bootloader is the one operation here with no cheap recovery,
	// so the normal case — an update that does not change U-Boot — should not
	// touch the region at all.
	if same, err := b.ubootMatches(imxData); err != nil {
		return fmt.Errorf("compare existing U-Boot: %w", err)
	} else if same {
		b.logger.Printf("[boot] U-Boot already at %s, not rewriting", expectedHash)
		return nil
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	// Even a failed unlock may have changed sysfs; always attempt to re-lock.
	defer func() {
		if err := b.io.setReadOnly(b.forceROPath, true); err != nil {
			result = errors.Join(result, fmt.Errorf("re-lock boot region: %w", err))
		}
	}()
	if err := b.io.setReadOnly(b.forceROPath, false); err != nil {
		return fmt.Errorf("unlock boot region: %w", err)
	}

	f, err := b.openRegion(os.O_RDWR, offset, extent)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, f.Close()) }()
	if err := ctx.Err(); err != nil {
		return err
	}
	// Once the write begins, finish syncing and verifying even if cancelled;
	// stopping halfway would leave a partially replaced bootloader.
	if n, err := f.WriteAt(imxData, offset); err != nil {
		return fmt.Errorf("write %s: %w", b.bootDevice, err)
	} else if n != len(imxData) {
		return io.ErrShortWrite
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", b.bootDevice, err)
	}
	if same, err := matches(f, imxData, offset); err != nil {
		return fmt.Errorf("read back U-Boot: %w", err)
	} else if !same {
		return fmt.Errorf("verify U-Boot: sha256 mismatch (expected %s)", expectedHash)
	}

	b.logger.Printf("[boot] U-Boot written and verified (%d bytes at offset %d)", len(imxData), offset)
	return nil
}

// validateIMX checks the supported image structure, not board compatibility or
// bootability. Packaged checksums detect corruption that structure cannot.
// The source IVT starts at file offset zero; the target offset is separate.
func validateIMX(data []byte) error {
	_, err := imxExtent(data)
	return err
}

func sha256sum(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
