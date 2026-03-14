package delta

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ApplyChain applies a chain of delta patches: unpack once → apply all → repack once.
// Single-delta is just a chain of 1.
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

	// Step 1: Extract old.mender
	progress(2, fmt.Sprintf("Extracting old mender file (chain of %d deltas)", n))
	if err := UnpackMender(oldMenderPath, outputDir); err != nil {
		return fmt.Errorf("unpack old mender: %w", err)
	}

	compressedPayload := filepath.Join(dataDir, "0000.tar.gz")
	payloadPath := filepath.Join(work, "payload")

	// Step 2: Decompress payload, delete compressed
	progress(5, "Decompressing payload")
	if err := ShellGunzip(compressedPayload, payloadPath); err != nil {
		return fmt.Errorf("decompress payload: %w", err)
	}
	os.Remove(compressedPayload)

	var lastMetadata *DeltaMetadata

	// Step 3: Apply each delta
	for i, deltaPath := range deltaPaths {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		deltaNum := i + 1
		pctStart := 8 + (i*67)/n
		pctEnd := 8 + (deltaNum*67)/n

		progress(pctStart, fmt.Sprintf("[Delta %d/%d] Extracting delta patch", deltaNum, n))

		deltaDir := filepath.Join(workDir, fmt.Sprintf("delta_%d", i))
		if err := os.MkdirAll(deltaDir, 0755); err != nil {
			return err
		}

		if err := ShellTarExtract(deltaPath, deltaDir); err != nil {
			return fmt.Errorf("extract delta %d: %w", deltaNum, err)
		}

		// Parse metadata
		metaBytes, err := os.ReadFile(filepath.Join(deltaDir, "metadata.json"))
		if err != nil {
			return fmt.Errorf("read metadata delta %d: %w", deltaNum, err)
		}
		var metadata DeltaMetadata
		if err := json.Unmarshal(metaBytes, &metadata); err != nil {
			return fmt.Errorf("parse metadata delta %d: %w", deltaNum, err)
		}
		lastMetadata = &metadata

		// Process changes
		for relPath, change := range metadata.Changes {
			outputFilePath := filepath.Join(outputDir, relPath)
			os.MkdirAll(filepath.Dir(outputFilePath), 0755)

			switch change.Type {
			case "new":
				srcPath := filepath.Join(deltaDir, "new_files", relPath)
				progress(pctStart+1, fmt.Sprintf("[Delta %d/%d] new: %s", deltaNum, n, relPath))
				if err := copyFile(srcPath, outputFilePath); err != nil {
					return fmt.Errorf("copy new file %s: %w", relPath, err)
				}

			case "modified":
				patchPath := filepath.Join(deltaDir, "patches", change.Patch)

				// Apply xdelta with simultaneous SHA256
				nextPayload := filepath.Join(work, "payload.next")
				progress(pctStart+2, fmt.Sprintf("[Delta %d/%d] patch: %s - applying xdelta", deltaNum, n, relPath))

				sha256hex, err := ApplyXdelta(ctx, payloadPath, patchPath, nextPayload)
				if err != nil {
					return fmt.Errorf("xdelta delta %d %s: %w", deltaNum, relPath, err)
				}

				// Verify checksum
				if change.NewMeta.DecompressedSHA256 != "" && sha256hex != change.NewMeta.DecompressedSHA256 {
					os.Remove(nextPayload)
					return fmt.Errorf("checksum mismatch after delta %d for %s: got %s, want %s",
						deltaNum, relPath, sha256hex, change.NewMeta.DecompressedSHA256)
				}

					// Rotate: delete old, rename new → current
				os.Remove(payloadPath)
				if err := os.Rename(nextPayload, payloadPath); err != nil {
					return fmt.Errorf("rename payload: %w", err)
				}

				pctMid := pctStart + (pctEnd-pctStart)*2/3
				progress(pctMid, fmt.Sprintf("[Delta %d/%d] patch: %s - verified", deltaNum, n, relPath))

			case "deleted":
				os.Remove(outputFilePath)
			}
		}

		// Clean up extracted delta
		os.RemoveAll(deltaDir)
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Step 4: Compress payload + compute rootfs checksum in a single pass.
	// Reads the ~1GB payload once, simultaneously gzipping and hashing the
	// rootfs image inside the tar. Saves one full read vs doing them separately.
	progress(75, "Compressing payload + computing rootfs checksum")
	rootfsChecksum, err := CompressPayloadAndHash(payloadPath, compressedPayload)
	if err != nil {
		return fmt.Errorf("compress payload: %w", err)
	}

	// Delete decompressed payload (free ~1GB)
	os.Remove(payloadPath)

	// Step 5: Verify against metadata
	if lastMetadata != nil && lastMetadata.NewPayloadChecksum != "" {
		if rootfsChecksum != lastMetadata.NewPayloadChecksum {
			return fmt.Errorf("rootfs checksum mismatch: got %s, want %s",
				rootfsChecksum, lastMetadata.NewPayloadChecksum)
		}
		a.logger.Printf("Rootfs checksum verified: %s", rootfsChecksum)
	}

	// Step 6: Update header.tar.gz
	progress(87, "Updating header metadata")
	headerPath := filepath.Join(outputDir, "header.tar.gz")
	if err := UpdateHeaderChecksum(headerPath, work, rootfsChecksum); err != nil {
		return fmt.Errorf("update header: %w", err)
	}

	// Step 7: Regenerate manifest
	progress(92, "Generating manifest")
	if err := GenerateManifest(outputDir); err != nil {
		return fmt.Errorf("generate manifest: %w", err)
	}

	// Step 8: Repack
	progress(95, "Creating output mender file")
	if err := RepackMender(outputDir, newMenderPath); err != nil {
		return fmt.Errorf("repack mender: %w", err)
	}

	progress(100, fmt.Sprintf("Complete (%d deltas applied)", n))
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

	if _, err := copyData(out, in); err != nil {
		return err
	}
	return out.Close()
}

func copyData(dst *os.File, src *os.File) (int64, error) {
	return dst.ReadFrom(src)
}

