package delta

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

// ErrBaseMismatch reports a delta that was built against a different base image
// than the one it is about to be applied to. Applying it would fail anyway, in
// xdelta3's window checksum, but only after the base has been unpacked and its
// payload decompressed.
var ErrBaseMismatch = errors.New("delta does not apply to this base image")

// ReadMetadata reads a delta's metadata.json without unpacking its patches.
//
// mender-delta-create.py writes metadata.json as the archive's first member for
// exactly this reason, but the member is located by name rather than by
// position so that deltas built before that ordering was pinned still read.
func ReadMetadata(deltaPath string) (*DeltaMetadata, error) {
	f, err := os.Open(deltaPath)
	if err != nil {
		return nil, fmt.Errorf("open delta: %w", err)
	}
	defer f.Close()

	// Deltas are gzipped tars, but ShellTarExtract accepts either, so tolerate
	// a plain tar here too rather than rejecting a file the applier would take.
	var r io.Reader = f
	gz, err := gzip.NewReader(f)
	if err == nil {
		defer gz.Close()
		r = gz
	} else if !errors.Is(err, gzip.ErrHeader) {
		return nil, fmt.Errorf("read delta: %w", err)
	} else if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind delta: %w", err)
	}

	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("delta has no metadata.json")
		}
		if err != nil {
			return nil, fmt.Errorf("read delta: %w", err)
		}
		if path.Clean(hdr.Name) != "metadata.json" {
			continue
		}
		var meta DeltaMetadata
		if err := json.NewDecoder(tr).Decode(&meta); err != nil {
			return nil, fmt.Errorf("parse metadata.json: %w", err)
		}
		return &meta, nil
	}
}

// BaseRootfsChecksum returns the SHA256 of the uncompressed rootfs recorded in
// a .mender artifact's manifest, which is the value a delta records as its
// old_payload_checksum.
//
// The manifest is the second member of a v3 artifact, ahead of the payload, so
// this normally reads a couple of kilobytes of a several-hundred-megabyte file.
// Should it appear later, tar seeks past the payload rather than reading it.
func BaseRootfsChecksum(menderPath string) (string, error) {
	f, err := os.Open(menderPath)
	if err != nil {
		return "", fmt.Errorf("open artifact: %w", err)
	}
	defer f.Close()

	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return "", errors.New("artifact has no manifest")
		}
		if err != nil {
			return "", fmt.Errorf("read artifact: %w", err)
		}
		if path.Clean(hdr.Name) != "manifest" {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return "", fmt.Errorf("read manifest: %w", err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			// Same shape the applier verifies against in
			// VerifyPayloadAgainstManifest: "<sha256>  data/NNNN/<file>".
			// A rootfs-image artifact carries exactly one such entry.
			fields := strings.Fields(line)
			if len(fields) == 2 && strings.HasPrefix(fields[1], "data/") {
				return fields[0], nil
			}
		}
		return "", errors.New("manifest has no data/ entry")
	}
}

// verifyChainApplies checks that every delta in the chain was built against the
// image the one before it produces, starting from the base artifact on disk.
// It reads only metadata, so a chain that cannot apply is rejected before the
// base is unpacked.
//
// Deltas predating the payload checksum fields leave a link unverifiable. Those
// are logged and skipped rather than rejected: a missing checksum says nothing
// about whether the delta applies.
func (a *Applier) verifyChainApplies(baseMenderPath string, deltaPaths []string) error {
	want, err := BaseRootfsChecksum(baseMenderPath)
	if err != nil {
		// An unreadable base is the unpack's problem to report, with the
		// better error message. Do not turn it into a mismatch here.
		a.logger.Printf("Cannot read base rootfs checksum from %s: %v (skipping base check)", baseMenderPath, err)
		return nil
	}

	for i, deltaPath := range deltaPaths {
		meta, err := ReadMetadata(deltaPath)
		if err != nil {
			return fmt.Errorf("read metadata delta %d/%d: %w", i+1, len(deltaPaths), err)
		}

		switch {
		case meta.OldPayloadChecksum == "":
			a.logger.Printf("[Delta %d/%d] no old_payload_checksum, cannot check base", i+1, len(deltaPaths))
		case want == "":
			a.logger.Printf("[Delta %d/%d] previous delta declared no new_payload_checksum, cannot check base", i+1, len(deltaPaths))
		case meta.OldPayloadChecksum != want:
			return fmt.Errorf("%w: delta %d/%d expects base rootfs %s, have %s",
				ErrBaseMismatch, i+1, len(deltaPaths), meta.OldPayloadChecksum, want)
		}

		want = meta.NewPayloadChecksum
	}

	a.logger.Printf("Delta chain applies to base (%d deltas)", len(deltaPaths))
	return nil
}
