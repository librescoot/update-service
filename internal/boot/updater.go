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
	mountPoint  string // e.g. /uboot
	bootDevice  string // e.g. /dev/mmcblk3boot0
	forceROPath string // e.g. /sys/block/mmcblk3boot0/force_ro
	dtbFile     string // e.g. librescoot-dbc.dtb
	versionFile string // e.g. /uboot/boot-version
	ubootSeek   int64  // 512-byte blocks to skip before writing U-Boot (default 2)
	logger      *log.Logger
}

// New creates a BootUpdater from the given parameters.
func New(mountPoint, bootDevice, dtbFile string, ubootSeek int64, logger *log.Logger) *BootUpdater {
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
		dtbFile:     dtbFile,
		versionFile: mountPoint + "/boot-version",
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

// CheckLocalAssets reads the version file from local boot assets baked into the rootfs.
// Returns ("", nil) if local assets are not available.
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
func (b *BootUpdater) WriteVersionFile(version string) error {
	if err := os.WriteFile(b.versionFile, []byte(version+"\n"), 0644); err != nil {
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
	b.logger.Printf("[boot] writing U-Boot: %s → %s", imxPath, b.bootDevice)
	if err := b.writeUBoot(imxPath); err != nil {
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

// writeUBoot unlocks force_ro, seeks to ubootSeek*512 bytes, writes imx data,
// reads back and verifies sha256, then re-locks force_ro.
func (b *BootUpdater) writeUBoot(imxPath string) error {
	imxData, err := os.ReadFile(imxPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", imxPath, err)
	}
	expectedHash := sha256sum(imxData)

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
	f.Close()

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

func sha256sum(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
