package delta

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ApplyChain applies a chain of delta patches: unpack once → apply all → repack once.
// Progress is based on total bytes processed across all I/O phases.
func (a *Applier) ApplyChain(ctx context.Context, oldMenderPath string, deltaPaths []string, newMenderPath string, progress ProgressCallback) error {
	n := len(deltaPaths)
	if n == 0 {
		return fmt.Errorf("no deltas provided")
	}

	workDir, err := os.MkdirTemp(a.tempDir, "delta-chain-*")
	if err != nil {
		return fmt.Errorf("create work dir: %w", err)
	}
	defer os.RemoveAll(workDir)

	outputDir := filepath.Join(workDir, "output")
	dataDir := filepath.Join(outputDir, "data")
	work := filepath.Join(workDir, "work")
	for _, d := range []string{outputDir, dataDir, work} {
		if err := os.MkdirAll(d, 0755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	// Extract old.mender (fast, no tracking)
	a.logger.Printf("Extracting old mender (chain of %d deltas)", n)
	if err := UnpackMender(oldMenderPath, outputDir); err != nil {
		return fmt.Errorf("unpack old mender: %w", err)
	}

	compressedPayload := filepath.Join(dataDir, "0000.tar.gz")
	payloadPath := filepath.Join(work, "payload")

	// Estimate total bytes of work:
	//   decompress output ≈ compressedSize * 3.5
	//   xdelta output per hop ≈ same as decompressed
	//   compress input = decompressed size
	//   manifest = hash all output files ≈ compressedSize
	compressedInfo, _ := os.Stat(compressedPayload)
	compressedSize := int64(0)
	if compressedInfo != nil {
		compressedSize = compressedInfo.Size()
	}
	estimatedPayload := compressedSize * 7 / 2
	totalWork := estimatedPayload*(int64(n)+2) + compressedSize

	tracker := newProgressTracker(totalWork, progress)

	// Decompress payload (slow — tracked)
	if err := ShellGunzipTracked(compressedPayload, payloadPath, tracker); err != nil {
		return fmt.Errorf("decompress payload: %w", err)
	}
	os.Remove(compressedPayload)

	var lastMetadata *DeltaMetadata

	// Apply each delta (slow — tracked)
	for i, deltaPath := range deltaPaths {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		deltaNum := i + 1
		a.logger.Printf("[Delta %d/%d] applying", deltaNum, n)

		deltaDir := filepath.Join(workDir, fmt.Sprintf("delta_%d", i))
		if err := os.MkdirAll(deltaDir, 0755); err != nil {
			return err
		}
		if err := ShellTarExtract(deltaPath, deltaDir); err != nil {
			return fmt.Errorf("extract delta %d: %w", deltaNum, err)
		}

		metaBytes, err := os.ReadFile(filepath.Join(deltaDir, "metadata.json"))
		if err != nil {
			return fmt.Errorf("read metadata delta %d: %w", deltaNum, err)
		}
		var metadata DeltaMetadata
		if err := json.Unmarshal(metaBytes, &metadata); err != nil {
			return fmt.Errorf("parse metadata delta %d: %w", deltaNum, err)
		}
		lastMetadata = &metadata

		for relPath, change := range metadata.Changes {
			outputFilePath := filepath.Join(outputDir, relPath)
			os.MkdirAll(filepath.Dir(outputFilePath), 0755)

			switch change.Type {
			case "new":
				if err := copyFile(filepath.Join(deltaDir, "new_files", relPath), outputFilePath); err != nil {
					return fmt.Errorf("copy new file %s: %w", relPath, err)
				}

			case "modified":
				patchPath := filepath.Join(deltaDir, "patches", change.Patch)
				nextPayload := filepath.Join(work, "payload.next")

				sha256hex, err := ApplyXdelta(ctx, payloadPath, patchPath, nextPayload, tracker)
				if err != nil {
					return fmt.Errorf("xdelta delta %d %s: %w", deltaNum, relPath, err)
				}

				if change.NewMeta.DecompressedSHA256 != "" && sha256hex != change.NewMeta.DecompressedSHA256 {
					os.Remove(nextPayload)
					return fmt.Errorf("checksum mismatch after delta %d for %s: got %s, want %s",
						deltaNum, relPath, sha256hex, change.NewMeta.DecompressedSHA256)
				}

				os.Remove(payloadPath)
				if err := os.Rename(nextPayload, payloadPath); err != nil {
					return fmt.Errorf("rename payload: %w", err)
				}

			case "deleted":
				os.Remove(outputFilePath)
			}
		}

		os.RemoveAll(deltaDir)
		a.logger.Printf("[Delta %d/%d] verified", deltaNum, n)
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Compress + rootfs checksum (slow — tracked)
	rootfsChecksum, err := CompressPayloadAndHash(payloadPath, compressedPayload, tracker)
	if err != nil {
		return fmt.Errorf("compress payload: %w", err)
	}
	os.Remove(payloadPath)

	if lastMetadata != nil && lastMetadata.NewPayloadChecksum != "" {
		if rootfsChecksum != lastMetadata.NewPayloadChecksum {
			return fmt.Errorf("rootfs checksum mismatch: got %s, want %s",
				rootfsChecksum, lastMetadata.NewPayloadChecksum)
		}
		a.logger.Printf("Rootfs checksum verified: %s", rootfsChecksum)
	}

	// Finalize (manifest is slow for large payload — tracked)
	headerPath := filepath.Join(outputDir, "header.tar.gz")
	if err := UpdateHeaderChecksum(headerPath, work, rootfsChecksum); err != nil {
		return fmt.Errorf("update header: %w", err)
	}

	if err := GenerateManifest(outputDir, tracker); err != nil {
		return fmt.Errorf("generate manifest: %w", err)
	}

	if err := RepackMender(outputDir, newMenderPath); err != nil {
		return fmt.Errorf("repack mender: %w", err)
	}

	progress(100, fmt.Sprintf("complete (%d deltas)", n))
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := out.ReadFrom(in); err != nil {
		return err
	}
	return out.Close()
}
