package delta

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Approximate durations on ARM (measured on i.MX6UL with MDB ~142MB mender):
//   Extract mender:  13s
//   Decompress:     110s
//   xdelta per hop: 198s
//   Compress:       400s
//   Finalize:        30s
//
// Progress is allocated proportionally so each % point ≈ same wall time.
// Only the slow I/O phases (decompress, xdelta, compress) drive smooth
// progress via progressReader. Fast phases (extract, finalize) just log
// milestones without emitting progress.

const (
	durExtract    = 13
	durDecompress = 110
	durXdelta     = 198 // per delta
	durCompress   = 400
	durFinalize   = 48
)

// ApplyChain applies a chain of delta patches: unpack once → apply all → repack once.
func (a *Applier) ApplyChain(ctx context.Context, oldMenderPath string, deltaPaths []string, newMenderPath string, progress ProgressCallback) error {
	n := len(deltaPaths)
	if n == 0 {
		return fmt.Errorf("no deltas provided")
	}

	// Calculate proportional progress ranges
	totalDur := durExtract + durDecompress + durXdelta*n + durCompress + durFinalize
	pctAfterExtract := 1 + (durExtract*98)/totalDur
	pctAfterDecompress := pctAfterExtract + (durDecompress*98)/totalDur
	pctAfterDeltas := pctAfterDecompress + (durXdelta*n*98)/totalDur
	pctAfterCompress := pctAfterDeltas + (durCompress*98)/totalDur
	// finalize fills up to 100%

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

	// Extract old.mender (fast — no smooth progress needed)
	a.logger.Printf("Extracting old mender (chain of %d deltas)", n)
	progress(1, "extracting")
	if err := UnpackMender(oldMenderPath, outputDir); err != nil {
		return fmt.Errorf("unpack old mender: %w", err)
	}

	compressedPayload := filepath.Join(dataDir, "0000.tar.gz")
	payloadPath := filepath.Join(work, "payload")

	// Estimate decompressed size (~3.5x ratio)
	compressedInfo, _ := os.Stat(compressedPayload)
	var estimatedDecompressed int64
	if compressedInfo != nil {
		estimatedDecompressed = compressedInfo.Size() * 7 / 2
	}

	// Decompress payload (slow — smooth progress)
	if err := ShellGunzipWithProgress(compressedPayload, payloadPath, estimatedDecompressed, pctAfterExtract, pctAfterDecompress, progress); err != nil {
		return fmt.Errorf("decompress payload: %w", err)
	}
	os.Remove(compressedPayload)

	payloadInfo, _ := os.Stat(payloadPath)
	var payloadSize int64
	if payloadInfo != nil {
		payloadSize = payloadInfo.Size()
	}

	var lastMetadata *DeltaMetadata

	// Apply each delta (slow — smooth progress per hop)
	for i, deltaPath := range deltaPaths {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		deltaNum := i + 1
		hopStart := pctAfterDecompress + (i*(pctAfterDeltas-pctAfterDecompress))/n
		hopEnd := pctAfterDecompress + (deltaNum*(pctAfterDeltas-pctAfterDecompress))/n

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
				srcPath := filepath.Join(deltaDir, "new_files", relPath)
				if err := copyFile(srcPath, outputFilePath); err != nil {
					return fmt.Errorf("copy new file %s: %w", relPath, err)
				}

			case "modified":
				patchPath := filepath.Join(deltaDir, "patches", change.Patch)
				nextPayload := filepath.Join(work, "payload.next")

				// xdelta drives smooth progress for this hop's range
				sha256hex, err := ApplyXdelta(ctx, payloadPath, patchPath, nextPayload, payloadSize, hopStart, hopEnd, progress)
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
		a.logger.Printf("[Delta %d/%d] applied and verified", deltaNum, n)
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Compress + rootfs checksum (slow — smooth progress)
	rootfsChecksum, err := CompressPayloadAndHash(payloadPath, compressedPayload, pctAfterDeltas, pctAfterCompress, progress)
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

	// Finalize: header update (fast), manifest (slow — hashes all files), repack (fast)
	headerPath := filepath.Join(outputDir, "header.tar.gz")
	if err := UpdateHeaderChecksum(headerPath, work, rootfsChecksum); err != nil {
		return fmt.Errorf("update header: %w", err)
	}

	if err := GenerateManifest(outputDir, pctAfterCompress, 99, progress); err != nil {
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
