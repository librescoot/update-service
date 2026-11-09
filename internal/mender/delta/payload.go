package delta

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// PayloadFormat is how an artifact's data/0000 member is compressed.
type PayloadFormat string

const (
	PayloadGzip PayloadFormat = "gzip"
	PayloadZstd PayloadFormat = "zstd"
)

// ErrZstdUnavailable reports a zstd payload on a system without the zstd tool.
var ErrZstdUnavailable = errors.New("payload is zstd-compressed but zstd is not installed")

// fileName is the member's name inside data/.
func (f PayloadFormat) fileName() string {
	if f == PayloadZstd {
		return "0000.tar.zst"
	}
	return "0000.tar.gz"
}

// ZstdAvailable reports whether this system can compress and decompress zstd
// payloads itself. Images that carry the tool are the ones whose Mender client
// was built to read zstd artifacts, which is why callers also use it to decide
// whether to ask for a zstd artifact at all.
func ZstdAvailable() bool {
	_, err := lookPath("zstd")
	return err == nil
}

// payloadFormatOfMember maps a data/ member name to its compression.
func payloadFormatOfMember(name string) (PayloadFormat, bool) {
	switch path.Base(name) {
	case "0000.tar.gz":
		return PayloadGzip, true
	case "0000.tar.zst":
		return PayloadZstd, true
	}
	return "", false
}

// findPayload locates the compressed rootfs payload in an unpacked artifact.
func findPayload(dataDir string) (string, PayloadFormat, error) {
	for _, f := range []PayloadFormat{PayloadGzip, PayloadZstd} {
		p := filepath.Join(dataDir, f.fileName())
		if _, err := os.Stat(p); err == nil {
			return p, f, nil
		}
	}
	return "", "", fmt.Errorf("no data/0000.tar.gz or data/0000.tar.zst payload in %s", dataDir)
}

// BasePayloadFormat reads which compression a .mender artifact's rootfs payload
// uses, from the member names alone. Tar seeks past the payload, so this does
// not read the several hundred megabytes of it.
func BasePayloadFormat(menderPath string) (PayloadFormat, error) {
	f, err := os.Open(menderPath)
	if err != nil {
		return "", fmt.Errorf("open artifact: %w", err)
	}
	defer f.Close()

	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return "", errors.New("artifact has no data/0000 payload")
		}
		if err != nil {
			return "", fmt.Errorf("read artifact: %w", err)
		}
		if strings.HasPrefix(path.Clean(hdr.Name), "data/") {
			if format, ok := payloadFormatOfMember(hdr.Name); ok {
				return format, nil
			}
		}
	}
}

// decompressPayload writes the raw payload tar for a compressed member.
func decompressPayload(format PayloadFormat, inputFile, outputFile string, tracker *progressTracker) error {
	switch format {
	case PayloadGzip:
		return ShellGunzipTracked(inputFile, outputFile, tracker)
	case PayloadZstd:
		if !ZstdAvailable() {
			return ErrZstdUnavailable
		}
		return shellDecompressTracked("zstd", []string{"-dc", "-q", inputFile}, outputFile, tracker)
	}
	return fmt.Errorf("unknown payload format %q", format)
}
