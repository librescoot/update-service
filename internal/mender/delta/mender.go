package delta

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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

// UpdateHeaderChecksum extracts header.tar.gz, updates rootfs-image.checksum
// in type-info, and recreates header.tar.gz with strict ordering.
func UpdateHeaderChecksum(headerTarGzPath, workDir, rootfsChecksum string) error {
	headerDir := filepath.Join(workDir, "header_extract")
	if err := os.MkdirAll(headerDir, 0755); err != nil {
		return err
	}
	defer os.RemoveAll(headerDir)

	if err := ShellTarExtract(headerTarGzPath, headerDir); err != nil {
		return fmt.Errorf("extract header.tar.gz: %w", err)
	}

	// Find and update type-info
	var typeInfoPath string
	filepath.Walk(headerDir, func(path string, info os.FileInfo, err error) error {
		if err == nil && info.Name() == "type-info" {
			typeInfoPath = path
		}
		return nil
	})
	if typeInfoPath == "" {
		return fmt.Errorf("type-info not found in header.tar.gz")
	}

	data, err := os.ReadFile(typeInfoPath)
	if err != nil {
		return err
	}

	var typeInfo map[string]any
	if err := json.Unmarshal(data, &typeInfo); err != nil {
		return fmt.Errorf("parse type-info: %w", err)
	}

	provides, ok := typeInfo["artifact_provides"].(map[string]any)
	if !ok {
		return fmt.Errorf("type-info missing artifact_provides")
	}
	provides["rootfs-image.checksum"] = rootfsChecksum

	out, err := json.Marshal(typeInfo)
	if err != nil {
		return err
	}
	if err := os.WriteFile(typeInfoPath, out, 0644); err != nil {
		return err
	}

	// Recreate header.tar.gz with strict ordering
	return recreateHeaderTarGz(headerTarGzPath, headerDir)
}

func recreateHeaderTarGz(outputPath, sourceDir string) error {
	f, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer f.Close()

	gw := gzip.NewWriter(f)
	defer gw.Close()

	tw := tar.NewWriter(gw)
	defer tw.Close()

	// Collect all files
	type fileEntry struct {
		fullPath string
		arcName  string
	}
	var files []fileEntry

	filepath.Walk(sourceDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(sourceDir, path)
		files = append(files, fileEntry{path, rel})
		return nil
	})

	// Mender header ordering: header-info first, type-info second, meta-data third
	sort.Slice(files, func(i, j int) bool {
		return headerSortKey(files[i].arcName) < headerSortKey(files[j].arcName)
	})

	for _, fe := range files {
		info, err := os.Stat(fe.fullPath)
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = fe.arcName

		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if err := copyFileToWriter(tw, fe.fullPath); err != nil {
			return err
		}
	}

	return nil
}

func headerSortKey(name string) string {
	base := filepath.Base(name)
	switch base {
	case "header-info":
		return "0:" + name
	case "type-info":
		return "1:" + name
	case "meta-data":
		return "2:" + name
	default:
		return "9:" + name
	}
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
				usable := int64(n)
				if usable > innerRemaining {
					usable = innerRemaining
				}
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

// GenerateManifest creates the manifest file with SHA256 checksums of all files.
// Progress is reported based on bytes hashed vs total bytes.
func GenerateManifest(outputDir string, tracker *progressTracker) error {
	manifestPath := filepath.Join(outputDir, "manifest")

	var entries []string
	filepath.Walk(outputDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || info.Name() == "manifest" {
			return err
		}
		rel, _ := filepath.Rel(outputDir, path)
		checksum, err := fileSHA256(path)
		if err != nil {
			return err
		}
		entries = append(entries, fmt.Sprintf("%s  %s\n", checksum, rel))
		// Track bytes hashed
		tracker.add(info.Size(), "finalizing")
		return nil
	})

	sort.Strings(entries)
	return os.WriteFile(manifestPath, []byte(strings.Join(entries, "")), 0644)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
