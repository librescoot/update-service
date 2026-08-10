package mender

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// ErrArtifactTooLarge reports an artifact whose rootfs payload is larger than
// the partition it would be written to.
var ErrArtifactTooLarge = errors.New("rootfs payload does not fit the target partition")

// ArtifactInfo holds the parts of a Mender artifact we inspect before handing
// the file to mender-update.
type ArtifactInfo struct {
	// DeviceTypes is artifact_depends.device_type from header-info. Present
	// for completeness; mender-update enforces it itself, see
	// ArtifactMatchesContext in the client's context.cpp.
	DeviceTypes []string
	// PayloadName is the file inside data/0000, e.g. the .ext4 rootfs.
	PayloadName string
	// PayloadSize is that file's uncompressed size in bytes. This is the
	// number mender hands to mender-flash as --input-size.
	PayloadSize int64
}

// headerInfo mirrors the fields we read out of header-info.
type headerInfo struct {
	ArtifactDepends struct {
		DeviceType []string `json:"device_type"`
	} `json:"artifact_depends"`
}

// ReadArtifactInfo reads an artifact's device types and the uncompressed size
// of its rootfs payload.
//
// A version 3 artifact is a tar holding "version", "manifest", "header.tar.gz"
// and one "data/NNNN.tar.gz" per payload, in that order. The payload's
// uncompressed size is in the tar header of the file inside data/0000, so only
// the first few kilobytes of a several-hundred-megabyte member get read.
func ReadArtifactInfo(path string) (*ArtifactInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open artifact: %w", err)
	}
	defer f.Close()

	info := &ArtifactInfo{}
	var sawHeader, sawPayload bool

	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read artifact: %w", err)
		}

		switch {
		case hdr.Name == "header.tar.gz":
			types, err := readDeviceTypes(tr)
			if err != nil {
				return nil, err
			}
			info.DeviceTypes = types
			sawHeader = true

		case strings.HasPrefix(hdr.Name, "data/0000."):
			name, size, err := readPayloadEntry(hdr.Name, tr)
			if err != nil {
				return nil, err
			}
			info.PayloadName = name
			info.PayloadSize = size
			sawPayload = true
		}

		if sawHeader && sawPayload {
			break
		}
	}

	if !sawHeader {
		return nil, errors.New("artifact has no header.tar.gz")
	}
	if !sawPayload {
		return nil, errors.New("artifact has no data/0000 payload")
	}
	return info, nil
}

// readDeviceTypes pulls artifact_depends.device_type out of header.tar.gz.
func readDeviceTypes(r io.Reader) ([]string, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("open header.tar.gz: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("header.tar.gz has no header-info")
		}
		if err != nil {
			return nil, fmt.Errorf("read header.tar.gz: %w", err)
		}
		if hdr.Name != "header-info" {
			continue
		}
		var hi headerInfo
		if err := json.NewDecoder(tr).Decode(&hi); err != nil {
			return nil, fmt.Errorf("parse header-info: %w", err)
		}
		return hi.ArtifactDepends.DeviceType, nil
	}
}

// readPayloadEntry returns the name and uncompressed size of the first file in
// a data/NNNN member. The member is a tar, optionally gzipped; other
// compressions are reported as unsupported rather than guessed at.
func readPayloadEntry(memberName string, r io.Reader) (string, int64, error) {
	switch {
	case strings.HasSuffix(memberName, ".tar.gz"):
		gz, err := gzip.NewReader(r)
		if err != nil {
			return "", 0, fmt.Errorf("open %s: %w", memberName, err)
		}
		defer gz.Close()
		r = gz
	case strings.HasSuffix(memberName, ".tar"):
		// already plain
	default:
		return "", 0, fmt.Errorf("unsupported payload compression: %s", memberName)
	}

	hdr, err := tar.NewReader(r).Next()
	if err != nil {
		return "", 0, fmt.Errorf("read %s: %w", memberName, err)
	}
	return hdr.Name, hdr.Size, nil
}
