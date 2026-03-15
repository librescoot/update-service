package delta

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// ApplyXdelta applies an xdelta3 patch, writing the result to outputFile.
// Returns the SHA256 of the output computed during the write (no separate pass).
// expectedSize is the approximate output size for progress reporting (0 to skip).
func ApplyXdelta(ctx context.Context, sourceFile, patchFile, outputFile string, expectedSize int64, pctStart, pctEnd int, progress ProgressCallback) (string, error) {
	cmd := exec.CommandContext(ctx, "nice", "-n", "10", "xdelta3", "-d", "-c", "-s", sourceFile, patchFile)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("xdelta3 stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr

	outFile, err := os.Create(outputFile)
	if err != nil {
		return "", fmt.Errorf("create output: %w", err)
	}

	hasher := sha256.New()

	if err := cmd.Start(); err != nil {
		outFile.Close()
		return "", fmt.Errorf("xdelta3 start: %w", err)
	}

	// Wrap stdout with progress reporting
	var reader io.Reader = stdout
	if expectedSize > 0 && progress != nil {
		reader = newProgressReader(stdout, expectedSize, pctStart, pctEnd, "applying xdelta", progress)
	}

	_, copyErr := io.Copy(io.MultiWriter(outFile, hasher), reader)
	outFile.Close()

	if waitErr := cmd.Wait(); waitErr != nil {
		os.Remove(outputFile)
		return "", fmt.Errorf("xdelta3 failed: %w", waitErr)
	}
	if copyErr != nil {
		os.Remove(outputFile)
		return "", fmt.Errorf("xdelta3 output copy: %w", copyErr)
	}

	return hex.EncodeToString(hasher.Sum(nil)), nil
}
