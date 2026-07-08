package delta

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// UnpackMender extracts a .mender tar archive into extractDir.
func UnpackMender(menderPath, extractDir string) error {
	return ShellTarExtract(menderPath, extractDir)
}

// RepackMender creates a .mender tar from files in sourceDir.
// Enforces mender ordering: version, manifest, header.tar.gz, then data/*.
func RepackMender(sourceDir, menderPath string) error {
	f, err := os.Create(menderPath)
	if err != nil {
		return fmt.Errorf("create mender: %w", err)
	}
	defer f.Close()

	tw := tar.NewWriter(f)
	defer tw.Close()

	// Fixed ordering
	items := []string{"version", "manifest", "header.tar.gz"}

	dataDir := filepath.Join(sourceDir, "data")
	if entries, err := os.ReadDir(dataDir); err == nil {
		for _, e := range entries {
			items = append(items, filepath.Join("data", e.Name()))
		}
	}

	for _, item := range items {
		fullPath := filepath.Join(sourceDir, item)
		info, err := os.Stat(fullPath)
		if err != nil {
			return fmt.Errorf("stat %s: %w", item, err)
		}

		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return fmt.Errorf("tar header %s: %w", item, err)
		}
		hdr.Name = item

		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("write header %s: %w", item, err)
		}

		if err := copyFileToWriter(tw, fullPath); err != nil {
			return fmt.Errorf("copy %s: %w", item, err)
		}
	}

	return nil
}

// copyFileToWriter streams a file's contents into w without buffering the
// whole file in memory. Critical on memory-constrained targets (DBC has
// 512 MB RAM, no swap) where the recompressed payload is ~160 MB.
func copyFileToWriter(w io.Writer, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

// VerifyPayloadAgainstManifest checks that the reconstructed payload's rootfs
// checksum is listed as a data/ entry in the manifest shipped with the delta.
//
// The manifest and header.tar.gz come verbatim from the new artifact (the
// delta ships them as "new" files) and must not be regenerated here: mender
// validates each file inside data/NNNN.tar.gz against a manifest entry named
// data/NNNN/<filename> holding the checksum of the uncompressed content.
func VerifyPayloadAgainstManifest(outputDir, rootfsChecksum string) error {
	data, err := os.ReadFile(filepath.Join(outputDir, "manifest"))
	if err != nil {
		return fmt.Errorf("read shipped manifest: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.HasPrefix(fields[1], "data/") && fields[0] == rootfsChecksum {
			return nil
		}
	}
	return fmt.Errorf("reconstructed payload checksum %s is not listed in the shipped manifest", rootfsChecksum)
}

// CompressPayloadAndHash compresses a payload tar with gzip while computing
// the rootfs checksum (SHA256 of the first file inside the tar) in a single
// pass. Returns the rootfs checksum. This avoids reading the ~1GB payload
// twice (once for gzip, once for checksum).
func CompressPayloadAndHash(payloadTarPath, compressedPath string, tracker *progressTracker) (rootfsChecksum string, err error) {
	inFile, err := os.Open(payloadTarPath)
	if err != nil {
		return "", fmt.Errorf("open payload: %w", err)
	}
	defer inFile.Close()

	outFile, err := os.Create(compressedPath)
	if err != nil {
		return "", fmt.Errorf("create compressed: %w", err)
	}
	defer outFile.Close()

	gzipCmd := lowPriorityCommand("gzip", "-3", "-c")
	gzipCmd.Stdout = outFile
	gzipIn, err := gzipCmd.StdinPipe()
	if err != nil {
		return "", fmt.Errorf("gzip stdin pipe: %w", err)
	}

	if err := gzipCmd.Start(); err != nil {
		return "", fmt.Errorf("start gzip: %w", err)
	}

	// Wrap input with byte tracking
	inReader := tracker.reader(inFile, "compressing")

	// Read the tar header (first 512 bytes) to get the inner file size
	const tarBlock = 512
	header := make([]byte, tarBlock)
	if _, err := io.ReadFull(inReader, header); err != nil {
		gzipIn.Close()
		gzipCmd.Wait()
		return "", fmt.Errorf("read tar header: %w", err)
	}

	// Write header to gzip
	if _, err := gzipIn.Write(header); err != nil {
		gzipIn.Close()
		gzipCmd.Wait()
		return "", fmt.Errorf("write header to gzip: %w", err)
	}

	// Parse inner file size from UStar header (bytes 124-136, null-padded octal)
	sizeField := header[124:136]
	// Trim null bytes and spaces
	sizeStr := strings.TrimRight(string(sizeField), "\x00 ")
	innerSize, _ := strconv.ParseInt(sizeStr, 8, 64)

	// Stream remaining data through gzip while hashing the inner file content
	innerHasher := sha256.New()
	innerRemaining := innerSize
	buf := make([]byte, 64*1024)

	for {
		n, readErr := inReader.Read(buf)
		if n > 0 {
			// Write to gzip
			if _, err := gzipIn.Write(buf[:n]); err != nil {
				gzipIn.Close()
				gzipCmd.Wait()
				return "", fmt.Errorf("write to gzip: %w", err)
			}

			// Hash the inner file portion
			if innerRemaining > 0 {
				usable := min(int64(n), innerRemaining)
				innerHasher.Write(buf[:usable])
				innerRemaining -= usable
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			gzipIn.Close()
			gzipCmd.Wait()
			return "", fmt.Errorf("read payload: %w", readErr)
		}
	}

	gzipIn.Close()
	if err := gzipCmd.Wait(); err != nil {
		return "", fmt.Errorf("gzip failed: %w", err)
	}

	return hex.EncodeToString(innerHasher.Sum(nil)), nil
}

