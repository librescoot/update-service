package boot

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxIMXBytes      = 32 * 1024 * 1024
	maxManifestBytes = 64 * 1024
	manifestName     = "manifest.sha256"
)

// readUBootAsset verifies the packaged checksum before structural validation.
// The checksum detects payload corruption, not authenticity or bootability.
func readUBootAsset(path string) ([]byte, error) {
	manifest, err := readBoundedAsset(filepath.Join(filepath.Dir(path), manifestName), maxManifestBytes)
	if err != nil {
		return nil, fmt.Errorf("read boot manifest: %w", err)
	}
	expected, err := manifestUBootHash(manifest)
	if err != nil {
		return nil, err
	}
	data, err := readBoundedAsset(path, maxIMXBytes)
	if err != nil {
		return nil, fmt.Errorf("read U-Boot asset: %w", err)
	}
	actual := sha256.Sum256(data)
	if !bytes.Equal(actual[:], expected) {
		return nil, fmt.Errorf("U-Boot checksum differs from packaged manifest")
	}
	if err := validateIMX(data); err != nil {
		return nil, err
	}
	return data, nil
}

func readBoundedAsset(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.Join(fmt.Errorf("asset must be a regular file of at most %d bytes", limit), f.Close())
	}
	// Stat alone does not bound a file that grows while it is read.
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	err = errors.Join(err, f.Close())
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("asset exceeds %d bytes", limit)
	}
	return data, nil
}

func manifestUBootHash(data []byte) ([]byte, error) {
	if len(data) > maxManifestBytes {
		return nil, fmt.Errorf("boot manifest too large")
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 1024), maxManifestBytes+1)
	var expected []byte
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			continue
		}
		// sha256sum emits 64 hex digits, a space, a text/binary marker,
		// then the exact filename. Escaped filenames are not needed here.
		if len(line) < 67 || line[64] != ' ' || (line[65] != ' ' && line[65] != '*') || strings.ContainsAny(line, "\x00\r") {
			return nil, fmt.Errorf("malformed boot manifest entry")
		}
		hash, err := hex.DecodeString(line[:64])
		if err != nil {
			return nil, fmt.Errorf("malformed boot manifest checksum: %w", err)
		}
		if line[66:] == UBootPath {
			if expected != nil {
				return nil, fmt.Errorf("duplicate U-Boot manifest entry")
			}
			expected = hash
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read boot manifest entries: %w", err)
	}
	if expected == nil {
		return nil, fmt.Errorf("boot manifest missing exact %s entry", UBootPath)
	}
	return expected, nil
}
