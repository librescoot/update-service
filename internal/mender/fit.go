package mender

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// defaultMenderConfPaths are the files mender reads RootfsPartA/B from, in the
// order the rootfs-image update module reads them: least precedence first.
var defaultMenderConfPaths = []string{
	"/var/lib/mender/mender.conf",
	"/etc/mender/mender.conf",
}

type menderRootfsConf struct {
	RootfsPartA string
	RootfsPartB string
}

// rootfsPartitions returns the A/B rootfs device paths from mender's config.
// A later file's non-empty value overrides an earlier one, matching the module.
func rootfsPartitions(paths []string) (string, string, error) {
	var a, b string
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var conf menderRootfsConf
		if err := json.Unmarshal(data, &conf); err != nil {
			continue
		}
		if conf.RootfsPartA != "" {
			a = conf.RootfsPartA
		}
		if conf.RootfsPartB != "" {
			b = conf.RootfsPartB
		}
	}
	if a == "" || b == "" {
		return "", "", errors.New("no RootfsPartA/RootfsPartB in mender config")
	}
	return a, b, nil
}

// deviceSize returns the size in bytes of a block device. Seeking to the end of
// a block device yields its capacity, so this needs no ioctl and no cgo.
func deviceSize(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	n, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	if n <= 0 {
		return 0, fmt.Errorf("%s reports size %d", path, n)
	}
	return n, nil
}

// smallestRootfsSlot returns the size of the smaller of the two rootfs
// partitions. They are equal by construction; taking the smaller means a
// mismatched pair fails safe rather than passing on the larger one.
func (i *Installer) smallestRootfsSlot() (int64, error) {
	confPaths := i.menderConfPaths
	if confPaths == nil {
		confPaths = defaultMenderConfPaths
	}
	sizeOf := i.deviceSize
	if sizeOf == nil {
		sizeOf = deviceSize
	}

	a, b, err := rootfsPartitions(confPaths)
	if err != nil {
		return 0, err
	}

	sizeA, errA := sizeOf(a)
	sizeB, errB := sizeOf(b)
	switch {
	case errA != nil && errB != nil:
		return 0, fmt.Errorf("cannot size %s (%v) or %s (%v)", a, errA, b, errB)
	case errA != nil:
		return sizeB, nil
	case errB != nil:
		return sizeA, nil
	case sizeA < sizeB:
		return sizeA, nil
	default:
		return sizeB, nil
	}
}

// checkArtifactFits refuses an artifact whose rootfs payload is larger than the
// slot it would be written to.
//
// mender-flash does no such check: it writes --input-size bytes and lets the
// kernel stop it with ENOSPC at the partition boundary. That does fail the
// install rather than truncating silently, but only after the passive slot has
// been overwritten end to end, so the rollback copy is gone and the eMMC has
// taken a full write cycle for nothing.
//
// Anything that leaves the check unable to reach a verdict (unparsable
// artifact, unreadable mender config, unsizeable device) logs and returns nil.
// This is a safety net over a path that already works; it must not be the
// reason a good update is refused.
func (i *Installer) checkArtifactFits(filePath string) error {
	info, err := ReadArtifactInfo(filePath)
	if err != nil {
		i.logger.Printf("Fit check skipped, cannot read %s: %v", filePath, err)
		return nil
	}

	slot, err := i.smallestRootfsSlot()
	if err != nil {
		i.logger.Printf("Fit check skipped, cannot determine rootfs slot size: %v", err)
		return nil
	}

	if info.PayloadSize > slot {
		return fmt.Errorf("%w: %s is %d bytes, rootfs slot is %d",
			ErrArtifactTooLarge, info.PayloadName, info.PayloadSize, slot)
	}

	i.logger.Printf("Fit check passed: %s is %d bytes, rootfs slot is %d",
		info.PayloadName, info.PayloadSize, slot)
	return nil
}
