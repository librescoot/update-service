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
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

const LocalAssetsPath = "/usr/share/boot-assets"

// BootUpdater manages boot partition updates (zImage, DTB, U-Boot).
// U-Boot writes use eMMC boot partition A/B redundancy: write to the
// inactive partition (boot0 or boot1), verify, then flip PARTITION_CONFIG.
type BootUpdater struct {
	mountPoint  string // e.g. /uboot
	mmcDevice   string // e.g. /dev/mmcblk3 (base eMMC device)
	dtbFile     string // e.g. librescoot-dbc.dtb
	versionFile string // e.g. /uboot/boot-version
	ubootSeek   int64  // 512-byte blocks to skip before writing U-Boot (default 2)
	logger      *log.Logger
}

// bootPartRegex matches the "bootN" suffix on an eMMC boot partition device.
var bootPartRegex = regexp.MustCompile(`boot[01]$`)

// partitionConfigRegex captures the PARTITION_CONFIG hex value from
// `mmc extcsd read` output, e.g. "PARTITION_CONFIG: 0x48".
var partitionConfigRegex = regexp.MustCompile(`PARTITION_CONFIG:\s*0x([0-9A-Fa-f]+)`)

// New creates a BootUpdater from the given parameters. bootDevice may be the
// base eMMC device (e.g. /dev/mmcblk3) or a boot partition (/dev/mmcblk3boot0);
// the bootN suffix, if present, is stripped.
func New(mountPoint, bootDevice, dtbFile string, ubootSeek int64, logger *log.Logger) *BootUpdater {
	return &BootUpdater{
		mountPoint:  mountPoint,
		mmcDevice:   stripBootSuffix(bootDevice),
		dtbFile:     dtbFile,
		versionFile: mountPoint + "/boot-version",
		ubootSeek:   ubootSeek,
		logger:      logger,
	}
}

// stripBootSuffix removes a trailing "bootN" from an eMMC device path.
// "/dev/mmcblk3boot0" → "/dev/mmcblk3", "/dev/mmcblk3" → "/dev/mmcblk3".
func stripBootSuffix(dev string) string {
	return bootPartRegex.ReplaceAllString(dev, "")
}

// DetectBootDevice reads /proc/mounts, finds the device mounted at mountPoint,
// and returns the base eMMC device (with any partition suffix stripped).
// E.g.: /dev/mmcblk3p1 mounted at /uboot → /dev/mmcblk3.
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
		return base, nil
	}

	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("reading mounts: %w", err)
	}
	return "", fmt.Errorf("no device found mounted at %s", mountPoint)
}

// CheckLocalAssets reads the version file from local boot assets baked into
// the rootfs. Returns ("", nil) if local assets are not available.
func (b *BootUpdater) CheckLocalAssets() (string, error) {
	data, err := os.ReadFile(LocalAssetsPath + "/version")
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read local boot version: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// GetInstalledVersion reads the version file; returns "" if absent.
func (b *BootUpdater) GetInstalledVersion() (string, error) {
	data, err := os.ReadFile(b.versionFile)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", b.versionFile, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// WriteVersionFile writes the version string to the version file.
// Remounts the boot partition read-write if the initial write fails.
func (b *BootUpdater) WriteVersionFile(version string) error {
	data := []byte(version + "\n")
	err := os.WriteFile(b.versionFile, data, 0644)
	if err != nil {
		remountRO, rerr := b.remountRW()
		if rerr != nil {
			return fmt.Errorf("remount rw for version file: %w", rerr)
		}
		defer remountRO()
		err = os.WriteFile(b.versionFile, data, 0644)
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", b.versionFile, err)
	}
	return nil
}

// Apply extracts tarball contents and writes all boot assets with verification.
// Order: zImage → DTB → sync → U-Boot (write to hardware last).
func (b *BootUpdater) Apply(ctx context.Context, extractDir string) error {
	remountRO, err := b.remountRW()
	if err != nil {
		return fmt.Errorf("remount rw: %w", err)
	}
	defer remountRO()

	zImageSrc := extractDir + "/zImage"
	zImageDst := b.mountPoint + "/zImage"
	b.logger.Printf("[boot] writing zImage: %s → %s", zImageSrc, zImageDst)
	if err := writeFileVerified(zImageDst, zImageSrc); err != nil {
		return fmt.Errorf("write zImage: %w", err)
	}

	dtbSrc := extractDir + "/" + b.dtbFile
	dtbDst := b.mountPoint + "/" + b.dtbFile
	b.logger.Printf("[boot] writing DTB: %s → %s", dtbSrc, dtbDst)
	if err := writeFileVerified(dtbDst, dtbSrc); err != nil {
		return fmt.Errorf("write DTB: %w", err)
	}

	syscall.Sync()

	imxPath := extractDir + "/u-boot-dtb.imx"
	b.logger.Printf("[boot] writing U-Boot to inactive boot partition")
	if err := b.writeUBoot(ctx, imxPath); err != nil {
		return fmt.Errorf("write U-Boot: %w", err)
	}

	syscall.Sync()
	return nil
}

// remountRW remounts the mountPoint read-write and returns a function that
// remounts it read-only again.
func (b *BootUpdater) remountRW() (func(), error) {
	b.logger.Printf("[boot] remounting %s read-write", b.mountPoint)
	if err := syscall.Mount("", b.mountPoint, "", syscall.MS_REMOUNT, ""); err != nil {
		return nil, fmt.Errorf("remount rw %s: %w", b.mountPoint, err)
	}
	return func() {
		b.logger.Printf("[boot] remounting %s read-only", b.mountPoint)
		if err := syscall.Mount("", b.mountPoint, "", syscall.MS_REMOUNT|syscall.MS_RDONLY, ""); err != nil {
			b.logger.Printf("[boot] warning: failed to remount %s read-only: %v", b.mountPoint, err)
		}
	}, nil
}

// writeFileVerified copies src → dst, syncs, reads back and compares sha256.
func writeFileVerified(dst, src string) error {
	srcData, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read src %s: %w", src, err)
	}

	expectedHash := sha256sum(srcData)

	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("open dst %s: %w", dst, err)
	}
	if _, err := f.Write(srcData); err != nil {
		f.Close()
		return fmt.Errorf("write dst %s: %w", dst, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync dst %s: %w", dst, err)
	}
	f.Close()

	// Read back and verify
	dstData, err := os.ReadFile(dst)
	if err != nil {
		return fmt.Errorf("read back %s: %w", dst, err)
	}
	actualHash := sha256sum(dstData)
	if actualHash != expectedHash {
		return fmt.Errorf("verify %s: sha256 mismatch (expected %s, got %s)", dst, expectedHash, actualHash)
	}
	return nil
}

// writeUBoot writes the new U-Boot image to the inactive eMMC boot partition,
// verifies via SHA256, then flips PARTITION_CONFIG to boot from it.
// The previously active partition is left intact as a fallback.
func (b *BootUpdater) writeUBoot(ctx context.Context, imxPath string) error {
	imxData, err := os.ReadFile(imxPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", imxPath, err)
	}
	expectedHash := sha256sum(imxData)

	active, ack, err := readPartitionConfig(ctx, b.mmcDevice)
	if err != nil {
		return fmt.Errorf("read partition config: %w", err)
	}

	target := inactivePartition(active)
	targetDev := bootPartitionDevice(b.mmcDevice, target)
	b.logger.Printf("[boot] active partition=%d, writing to inactive partition %d (%s)", active, target, targetDev)

	if err := b.writeAndVerifyUBootImage(targetDev, imxData, expectedHash); err != nil {
		return err
	}

	b.logger.Printf("[boot] flipping PARTITION_CONFIG to boot from partition %d (ack=%d)", target, ack)
	if err := flipPartitionConfig(ctx, b.mmcDevice, target, ack); err != nil {
		return fmt.Errorf("flip partition config to %d: %w", target, err)
	}

	b.logger.Printf("[boot] U-Boot update complete: new partition=%d, fallback partition=%d", target, active)
	return nil
}

// writeAndVerifyUBootImage unlocks force_ro for the target boot partition,
// writes imxData at the configured seek offset, verifies via SHA256, then
// re-locks force_ro.
func (b *BootUpdater) writeAndVerifyUBootImage(targetDev string, imxData []byte, expectedHash string) error {
	forceROPath := forceROPathFor(targetDev)

	if err := os.WriteFile(forceROPath, []byte("0\n"), 0200); err != nil {
		return fmt.Errorf("unlock %s: %w", forceROPath, err)
	}
	defer func() {
		if err := os.WriteFile(forceROPath, []byte("1\n"), 0200); err != nil {
			b.logger.Printf("[boot] warning: failed to re-lock %s: %v", forceROPath, err)
		}
	}()

	f, err := os.OpenFile(targetDev, os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("open %s: %w", targetDev, err)
	}

	offset := b.ubootSeek * 512
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		f.Close()
		return fmt.Errorf("seek %s to %d: %w", targetDev, offset, err)
	}
	if _, err := f.Write(imxData); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", targetDev, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync %s: %w", targetDev, err)
	}
	f.Close()

	rf, err := os.Open(targetDev)
	if err != nil {
		return fmt.Errorf("open for verify %s: %w", targetDev, err)
	}
	defer rf.Close()

	if _, err := rf.Seek(offset, io.SeekStart); err != nil {
		return fmt.Errorf("seek for verify %s: %w", targetDev, err)
	}
	readBack := make([]byte, len(imxData))
	if _, err := io.ReadFull(rf, readBack); err != nil {
		return fmt.Errorf("read back %s: %w", targetDev, err)
	}
	actualHash := sha256sum(readBack)
	if actualHash != expectedHash {
		return fmt.Errorf("verify U-Boot on %s: sha256 mismatch (expected %s, got %s)", targetDev, expectedHash, actualHash)
	}

	b.logger.Printf("[boot] U-Boot written and verified on %s (%d bytes at offset %d)", targetDev, len(imxData), offset)
	return nil
}

// bootPartitionDevice returns the device path for boot partition n (1 or 2).
// /dev/mmcblk3 + 1 → /dev/mmcblk3boot0, + 2 → /dev/mmcblk3boot1.
func bootPartitionDevice(mmcDevice string, n int) string {
	return fmt.Sprintf("%sboot%d", mmcDevice, n-1)
}

// forceROPathFor returns the force_ro sysfs path for a boot partition device.
// /dev/mmcblk3boot1 → /sys/block/mmcblk3boot1/force_ro.
func forceROPathFor(bootDev string) string {
	return "/sys/block/" + strings.TrimPrefix(bootDev, "/dev/") + "/force_ro"
}

// inactivePartition returns the boot partition number (1 or 2) opposite to
// the currently active one. If no partition is active (active == 0) or the
// value is unexpected, defaults to partition 1 (boot0).
func inactivePartition(active int) int {
	switch active {
	case 1:
		return 2
	case 2:
		return 1
	default:
		return 1
	}
}

// readPartitionConfig runs `mmc extcsd read` and parses PARTITION_CONFIG.
// Returns the active boot partition (0, 1, or 2) and the current BOOT_ACK bit.
func readPartitionConfig(ctx context.Context, mmcDevice string) (active, ack int, err error) {
	out, err := exec.CommandContext(ctx, "mmc", "extcsd", "read", mmcDevice).Output()
	if err != nil {
		return 0, 0, fmt.Errorf("mmc extcsd read %s: %w", mmcDevice, err)
	}
	return parsePartitionConfig(string(out))
}

// parsePartitionConfig extracts PARTITION_CONFIG from `mmc extcsd read` output
// and returns the active partition (bits 5:3) and BOOT_ACK (bit 6).
func parsePartitionConfig(output string) (active, ack int, err error) {
	m := partitionConfigRegex.FindStringSubmatch(output)
	if m == nil {
		return 0, 0, fmt.Errorf("PARTITION_CONFIG not found in mmc extcsd output")
	}
	val, err := strconv.ParseUint(m[1], 16, 8)
	if err != nil {
		return 0, 0, fmt.Errorf("parse PARTITION_CONFIG value %q: %w", m[1], err)
	}
	active = int((val >> 3) & 0x07)
	ack = int((val >> 6) & 0x01)
	return active, ack, nil
}

// flipPartitionConfig invokes `mmc bootpart enable` to set the active boot
// partition. newPart is 1 (boot0) or 2 (boot1). ack is preserved from the
// current PARTITION_CONFIG state.
func flipPartitionConfig(ctx context.Context, mmcDevice string, newPart, ack int) error {
	if newPart != 1 && newPart != 2 {
		return fmt.Errorf("invalid boot partition %d (must be 1 or 2)", newPart)
	}
	cmd := exec.CommandContext(ctx, "mmc", "bootpart", "enable",
		strconv.Itoa(newPart), strconv.Itoa(ack), mmcDevice)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mmc bootpart enable %d %d %s: %w (output: %s)", newPart, ack, mmcDevice, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func sha256sum(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
