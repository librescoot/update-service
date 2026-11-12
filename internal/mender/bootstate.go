package mender

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// BootState separates the mounted rootfs from the bootloader's next selection.
type BootState struct {
	RunningSlot      string
	OtherSlot        string
	SelectedSlot     string
	UpgradeAvailable bool
}

// ReadBootState only accepts the configured A/B block devices on one disk.
func ReadBootState(ctx context.Context) (BootState, error) {
	a, b, err := rootfsPartitions(defaultMenderConfPaths)
	if err != nil {
		return BootState{}, err
	}
	a, err = filepath.EvalSymlinks(a)
	if err != nil {
		return BootState{}, err
	}
	b, err = filepath.EvalSymlinks(b)
	if err != nil {
		return BootState{}, err
	}
	root, err := deviceNumber("/", false)
	if err != nil {
		return BootState{}, err
	}
	da, err := deviceNumber(a, true)
	if err != nil {
		return BootState{}, err
	}
	db, err := deviceNumber(b, true)
	if err != nil {
		return BootState{}, err
	}
	if da == db || (root != da && root != db) {
		return BootState{}, fmt.Errorf("rootfs does not match a unique Mender slot")
	}
	pa, diskA, err := partitionInfo(da)
	if err != nil {
		return BootState{}, err
	}
	pb, diskB, err := partitionInfo(db)
	if err != nil {
		return BootState{}, err
	}
	if diskA != diskB || pa == pb {
		return BootState{}, fmt.Errorf("Mender slots are not distinct partitions on one disk")
	}
	output, err := exec.CommandContext(ctx, "fw_printenv", "mender_boot_part", "mender_boot_part_hex", "upgrade_available").Output()
	if err != nil {
		return BootState{}, fmt.Errorf("read boot environment: %w", err)
	}
	selected, upgrade, err := parseBootEnvironment(string(output))
	if err != nil {
		return BootState{}, err
	}
	state := BootState{UpgradeAvailable: upgrade}
	if root == da {
		state.RunningSlot, state.OtherSlot = a, b
	} else {
		state.RunningSlot, state.OtherSlot = b, a
	}
	switch selected {
	case pa:
		state.SelectedSlot = a
	case pb:
		state.SelectedSlot = b
	default:
		return BootState{}, fmt.Errorf("bootloader selected unknown partition %d", selected)
	}
	return state, nil
}

func deviceNumber(path string, block bool) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("no device metadata for %s", path)
	}
	if block {
		if info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0 {
			return 0, fmt.Errorf("%s is not a block device", path)
		}
		return uint64(stat.Rdev), nil
	}
	return uint64(stat.Dev), nil
}

func partitionInfo(dev uint64) (int, string, error) {
	path := filepath.Join("/sys/dev/block", fmt.Sprintf("%d:%d", unix.Major(dev), unix.Minor(dev)))
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return 0, "", err
	}
	data, err := os.ReadFile(filepath.Join(resolved, "partition"))
	if err != nil {
		return 0, "", err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || n <= 0 {
		return 0, "", fmt.Errorf("invalid partition number %q", data)
	}
	return n, filepath.Dir(resolved), nil
}

func parseBootEnvironment(output string) (int, bool, error) {
	values := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || (key != "mender_boot_part" && key != "mender_boot_part_hex" && key != "upgrade_available") {
			return 0, false, fmt.Errorf("unexpected boot environment line %q", line)
		}
		if _, exists := values[key]; exists {
			return 0, false, fmt.Errorf("duplicate boot environment key %s", key)
		}
		values[key] = value
	}
	selected, err := strconv.Atoi(values["mender_boot_part"])
	if err != nil || selected <= 0 {
		return 0, false, fmt.Errorf("invalid mender_boot_part %q", values["mender_boot_part"])
	}
	hex, err := strconv.ParseInt(values["mender_boot_part_hex"], 16, 32)
	if err != nil || int(hex) != selected {
		return 0, false, fmt.Errorf("boot partition decimal/hex mismatch")
	}
	switch values["upgrade_available"] {
	case "0":
		return selected, false, nil
	case "1":
		return selected, true, nil
	default:
		return 0, false, fmt.Errorf("invalid upgrade_available %q", values["upgrade_available"])
	}
}

// RootfsOrigin is Mender's own rollback destination and the identities saved
// with it. Never execute the orig-part file as shell code.
type RootfsOrigin struct {
	OriginalSlot      string
	Artifact          string
	CommittedArtifact string
}

// ErrNoRootfsOrigin distinguishes a pre-staging crash from an incomplete
// module tree that already contains a rollback destination.
var ErrNoRootfsOrigin = errors.New("rootfs module has no rollback destination")

func ReadRootfsOrigin() (RootfsOrigin, error) {
	return readRootfsOrigin("/var/lib/mender/modules/v3/payloads/0000/tree", defaultMenderConfPaths)
}

func readRootfsOrigin(tree string, conf []string) (RootfsOrigin, error) {
	data, err := os.ReadFile(filepath.Join(tree, "tmp/orig-part"))
	if errors.Is(err, os.ErrNotExist) {
		return RootfsOrigin{}, ErrNoRootfsOrigin
	}
	if err != nil {
		return RootfsOrigin{}, err
	}
	original, err := parseOriginalPartition(string(data))
	if err != nil {
		return RootfsOrigin{}, err
	}
	a, b, err := rootfsPartitions(conf)
	if err != nil {
		return RootfsOrigin{}, err
	}
	slot := ""
	for _, path := range []string{a, b} {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return RootfsOrigin{}, err
		}
		// rootfs-image derives these numbers from the configured device name.
		suffix := resolved[strings.LastIndexFunc(resolved, func(r rune) bool { return r < '0' || r > '9' })+1:]
		n, err := strconv.Atoi(suffix)
		if err != nil {
			return RootfsOrigin{}, err
		}
		if n == original {
			if slot != "" {
				return RootfsOrigin{}, fmt.Errorf("ambiguous original partition")
			}
			slot = resolved
		}
	}
	if slot == "" {
		return RootfsOrigin{}, fmt.Errorf("original partition is not a Mender slot")
	}
	read := func(name string) (string, error) {
		data, err := os.ReadFile(filepath.Join(tree, name))
		return strings.TrimSpace(string(data)), err
	}
	artifact, err := read("header/artifact_name")
	if err != nil {
		return RootfsOrigin{}, err
	}
	committed, err := read("current_artifact_name")
	if err != nil {
		return RootfsOrigin{}, err
	}
	if artifact == "" || committed == "" {
		return RootfsOrigin{}, fmt.Errorf("module tree lacks artifact identities")
	}
	return RootfsOrigin{OriginalSlot: slot, Artifact: artifact, CommittedArtifact: committed}, nil
}

func parseOriginalPartition(data string) (int, error) {
	values := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(data), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok || (k != "orig_part_num" && k != "orig_part_num_hex") {
			return 0, fmt.Errorf("invalid original partition record")
		}
		if _, ok := values[k]; ok {
			return 0, fmt.Errorf("duplicate original partition key")
		}
		values[k] = v
	}
	n, err := strconv.Atoi(values["orig_part_num"])
	h, hexErr := strconv.ParseInt(values["orig_part_num_hex"], 16, 32)
	if err != nil || hexErr != nil || n <= 0 || int(h) != n {
		return 0, fmt.Errorf("invalid original partition numbers")
	}
	return n, nil
}
