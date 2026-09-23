package boot

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
)

// TargetKind is the shape of the region U-Boot is written to. The two boards
// are fused differently, so the same updater writes a different region on each.
type TargetKind int

const (
	// regionUnknown is the zero value: a path the updater cannot classify.
	// validateWrite refuses it rather than guessing a region.
	regionUnknown TargetKind = iota

	// regionBootPartition is /dev/mmcblkNboot0, the eMMC boot partition 1.
	// Linux exposes it read-only until /sys/block/<dev>/force_ro is cleared.
	regionBootPartition

	// regionUserArea is /dev/mmcblkN, the eMMC user area, where U-Boot sits at
	// byte 1024 ahead of the first partition.
	regionUserArea
)

func (k TargetKind) String() string {
	switch k {
	case regionBootPartition:
		return "eMMC boot partition"
	case regionUserArea:
		return "eMMC user area"
	}
	return "unknown region"
}

// RegionKind reports the region a boot device path refers to, for logging and
// status. It never fails: an unrecognised path reports regionUnknown.
func RegionKind(path string) TargetKind {
	kind, _ := kindOfDevicePath(path)
	return kind
}

// Updater is the write side of a boot update: a set of regions to keep current.
type Updater interface {
	UpToDate(assetDir string) (bool, error)
	Apply(ctx context.Context, extractDir string) error
}

// Targets keeps U-Boot current in the region a board boots it from.
//
// That region is the eMMC user area on both machines, at byte 1024. Measured on
// the bench: with boot partition 1 overwritten and unchanged across the reboot,
// both the MDB and the DBC came up normally, while the user area held exactly the
// U-Boot that this updater had written there. The flash path agrees — the sdimg
// carries u-boot-dtb.imx at byte 1024 (MENDER_IMAGE_BOOTLOADER_BOOTSECTOR_OFFSET
// is 2), the flashing tool writes the image's boot area, and the environment
// lives in the same gap at 8 MiB and 16 MiB.
//
// Boot partition 1 stays a supported target for an explicit --boot-device, with
// its force_ro gate and its own size bound, but nothing resolves to it: writing
// it is what made DBC updates silent for as long as they were.
type Targets struct {
	updaters []*BootUpdater
	logger   *log.Logger
}

// NewTargets builds one updater per device.
func NewTargets(mountPoint string, devices []string, ubootSeek int64, logger *log.Logger) *Targets {
	updaters := make([]*BootUpdater, 0, len(devices))
	for _, device := range devices {
		updaters = append(updaters, New(mountPoint, device, ubootSeek, logger))
	}
	return &Targets{updaters: updaters, logger: logger}
}

// UpToDate reports whether every target already holds the packaged U-Boot.
func (t *Targets) UpToDate(assetDir string) (bool, error) {
	for _, updater := range t.updaters {
		same, err := updater.UpToDate(assetDir)
		if err != nil {
			return false, fmt.Errorf("%s: %w", updater.bootDevice, err)
		}
		if !same {
			return false, nil
		}
	}
	return true, nil
}

// Apply writes every target. A failure stops the run: the remaining regions
// would be left on a different U-Boot than the one that failed.
func (t *Targets) Apply(ctx context.Context, extractDir string) error {
	for _, updater := range t.updaters {
		if err := updater.Apply(ctx, extractDir); err != nil {
			return err
		}
	}
	return nil
}

// BootTargets lists every region U-Boot has to be current in on this machine.
func BootTargets(component, mountPoint string) ([]string, error) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil, fmt.Errorf("open /proc/mounts: %w", err)
	}
	defer f.Close()
	return bootTargetsFromReader(f, component, mountPoint)
}

func bootTargetsFromReader(mounts io.Reader, component, mountPoint string) ([]string, error) {
	base, err := storageDeviceFromReader(mounts, mountPoint)
	if err != nil {
		return nil, err
	}
	switch component {
	case "mdb", "dbc":
		return []string{base}, nil
	}
	return nil, fmt.Errorf("unknown component %q: cannot choose a U-Boot target", component)
}
