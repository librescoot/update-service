package boot

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

type regionFile interface {
	io.ReaderAt
	io.WriterAt
	Stat() (os.FileInfo, error)
	Sync() error
	Close() error
}

type bootIO struct {
	open           func(string, int) (regionFile, error)
	inspect        func(regionFile, string) (uint64, error)
	setReadOnly    func(string, bool) error
	partitionStart func(string) (uint64, error)
}

// Only boot partition 1 is a target: it is the one the MDB's ROM reads. The
// second boot partition exists on the hardware but nothing boots from it.
var (
	bootPartitionName = regexp.MustCompile(`^/dev/mmcblk[0-9]+boot0$`)
	wholeDeviceName   = regexp.MustCompile(`^/dev/mmcblk[0-9]+$`)
)

// kindOfDevicePath classifies a U-Boot write target by its device path.
func kindOfDevicePath(path string) (TargetKind, bool) {
	switch {
	case bootPartitionName.MatchString(path):
		return regionBootPartition, true
	case wholeDeviceName.MatchString(path):
		return regionUserArea, true
	}
	return regionUnknown, false
}

// firstPartitionStart returns the byte offset of the earliest partition on a
// whole block device. A U-Boot image written into the user area has to fit in
// the gap ahead of it.
func firstPartitionStart(device string) (uint64, error) {
	name := strings.TrimPrefix(device, "/dev/")
	base := "/sys/class/block/" + name
	entries, err := os.ReadDir(base)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", base, err)
	}
	prefix := name + "p"
	start := uint64(0)
	found := false
	for _, entry := range entries {
		part := entry.Name()
		if !strings.HasPrefix(part, prefix) {
			continue
		}
		if _, err := strconv.ParseUint(strings.TrimPrefix(part, prefix), 10, 32); err != nil {
			continue
		}
		raw, err := os.ReadFile(base + "/" + part + "/start")
		if err != nil {
			return 0, fmt.Errorf("read %s start: %w", part, err)
		}
		value, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
		if err != nil || value > math.MaxInt64/512 {
			return 0, fmt.Errorf("invalid start sector %q for %s", raw, part)
		}
		if bytes := value * 512; !found || bytes < start {
			start, found = bytes, true
		}
	}
	if !found {
		return 0, fmt.Errorf("no partitions found on %s", device)
	}
	return start, nil
}

func systemBootIO() bootIO {
	return bootIO{
		open: func(path string, flags int) (regionFile, error) {
			return os.OpenFile(path, flags|syscall.O_NOFOLLOW, 0)
		},
		inspect:        inspectRegion,
		partitionStart: firstPartitionStart,
		setReadOnly: func(path string, ro bool) error {
			f, err := os.OpenFile(path, os.O_WRONLY, 0)
			if err != nil {
				return err
			}
			value := "0\n"
			if ro {
				value = "1\n"
			}
			n, err := f.WriteString(value)
			if err == nil && n != len(value) {
				err = io.ErrShortWrite
			}
			return errors.Join(err, f.Close())
		},
	}
}

// Check the opened descriptor, not just its pathname. sysfs identifies the
// kernel's boot region and its capacity; ioctl independently checks capacity.
func inspectRegion(f regionFile, path string) (uint64, error) {
	if _, ok := kindOfDevicePath(path); !ok {
		return 0, fmt.Errorf("unsupported boot region %q", path)
	}
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0 {
		return 0, fmt.Errorf("target is not a block device")
	}
	base := "/sys/class/block/" + strings.TrimPrefix(path, "/dev/") + "/"
	dev, err := os.ReadFile(base + "dev")
	if err != nil {
		return 0, err
	}
	sectors, err := os.ReadFile(base + "size")
	if err != nil {
		return 0, err
	}
	size, err := validateRegionMetadata(uint64(st.Rdev), string(dev), string(sectors))
	if err != nil {
		return 0, err
	}
	fd, ok := f.(interface{ Fd() uintptr })
	if !ok {
		return 0, fmt.Errorf("missing device descriptor")
	}
	var capacity uint64
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd.Fd(), uintptr(unix.BLKGETSIZE64), uintptr(unsafe.Pointer(&capacity)))
	if errno != 0 {
		return 0, fmt.Errorf("boot region capacity: %w", errno)
	}
	if capacity != size {
		return 0, fmt.Errorf("boot region capacity disagrees with sysfs")
	}
	return size, nil
}

func validateRegionMetadata(rdev uint64, dev, sectors string) (uint64, error) {
	// Linux dev_t encoding, including the high minor bits.
	major := (rdev>>8)&0xfff | (rdev>>32)&0xfffff000
	minor := rdev&0xff | (rdev>>12)&0xffffff00
	if strings.TrimSpace(dev) != fmt.Sprintf("%d:%d", major, minor) {
		return 0, fmt.Errorf("boot region identity disagrees with sysfs")
	}
	count, err := strconv.ParseUint(strings.TrimSpace(sectors), 10, 64)
	if err != nil || count == 0 || count > math.MaxInt64/512 {
		return 0, fmt.Errorf("invalid boot region sector count %q", sectors)
	}
	return count * 512, nil
}
