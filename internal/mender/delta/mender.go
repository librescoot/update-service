package delta

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
//
// archive/tar does the header parsing rather than the size being read out of
// the first 512-byte block by hand. A payload is not necessarily plain ustar:
// pax and GNU both put an extra member ahead of the real one to carry a name
// over 100 bytes, and encode a size over 8GB outside the twelve octal digits
// the fixed header allows. Parsing those by hand hashes the wrong bytes and
// fails later as a rootfs checksum mismatch, blaming the payload.
//
// Every byte the parse consumes is teed into gzip, so gzip still receives the
// payload verbatim and the output is byte for byte what streaming it straight
// through would produce.
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

	// Closing the pipe releases gzip, which is otherwise left blocked on a
	// stdin that never ends.
	abort := func(cause error) (string, error) {
		_ = gzipIn.Close()
		_ = gzipCmd.Wait()
		return "", cause
	}

	tee := io.TeeReader(tracker.reader(inFile, "compressing"), gzipIn)
	tr := tar.NewReader(tee)

	if _, err := tr.Next(); err != nil {
		return abort(fmt.Errorf("read payload tar: %w", err))
	}

	innerHasher := sha256.New()
	if _, err := io.Copy(innerHasher, tr); err != nil {
		return abort(fmt.Errorf("hash payload: %w", err))
	}

	// The tar reader stops at the end of the first member's content. Drain
	// what follows (its padding, any later members, the end-of-archive
	// blocks) straight through the tee, since only gzip needs those.
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return abort(fmt.Errorf("copy payload: %w", err))
	}

	if err := gzipIn.Close(); err != nil {
		_ = gzipCmd.Wait()
		return "", fmt.Errorf("close gzip stdin: %w", err)
	}
	if err := gzipCmd.Wait(); err != nil {
		return "", fmt.Errorf("gzip failed: %w", err)
	}

	return hex.EncodeToString(innerHasher.Sum(nil)), nil
}
