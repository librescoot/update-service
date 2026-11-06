package delta

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

// ApplyHDiff reconstructs to a new file; the existing payload is never modified.
func ApplyHDiff(ctx context.Context, sourceFile, patchFile, outputFile string, tracker *progressTracker) (string, error) {
	bin, args := lowPriorityArgs("hpatchz", "-s-8m", sourceFile, patchFile, outputFile)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("hpatchz start: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var tracked int64
	for {
		select {
		case err := <-done:
			if err != nil {
				_ = os.Remove(outputFile)
				return "", fmt.Errorf("hpatchz failed: %w", err)
			}
			f, err := os.Open(outputFile)
			if err != nil {
				return "", fmt.Errorf("open hpatchz output: %w", err)
			}
			h := sha256.New()
			_, err = io.Copy(h, f)
			closeErr := f.Close()
			if err != nil {
				_ = os.Remove(outputFile)
				return "", fmt.Errorf("hash hpatchz output: %w", err)
			}
			if closeErr != nil {
				_ = os.Remove(outputFile)
				return "", fmt.Errorf("close hpatchz output: %w", closeErr)
			}
			if st, err := os.Stat(outputFile); err == nil && st.Size() > tracked {
				tracker.add(st.Size()-tracked, "applying hdiffpatch")
			}
			return hex.EncodeToString(h.Sum(nil)), nil
		case <-ticker.C:
			if st, err := os.Stat(outputFile); err == nil && st.Size() > tracked {
				tracker.add(st.Size()-tracked, "applying hdiffpatch")
				tracked = st.Size()
			}
		}
	}
}
