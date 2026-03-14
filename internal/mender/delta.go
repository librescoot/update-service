package mender

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/librescoot/update-service/internal/mender/delta"
)

const defaultTempDir = "/data/ota/tmp"

// DeltaApplier handles applying delta updates to generate new mender files
type DeltaApplier struct {
	logger  *log.Logger
	applier *delta.Applier
}

// NewDeltaApplier creates a new delta applier instance
func NewDeltaApplier(logger *log.Logger) *DeltaApplier {
	return &DeltaApplier{
		logger:  logger,
		applier: delta.NewApplier(logger, defaultTempDir),
	}
}

// DeltaProgressCallback is called with progress updates during delta application
// percent: 0-100 indicating progress
type DeltaProgressCallback func(percent int)

// ApplyDelta applies a single delta update (delegates to ApplyDeltaChain with one delta).
func (a *DeltaApplier) ApplyDelta(ctx context.Context, oldMenderPath, deltaPath, newMenderPath string, progressCallback DeltaProgressCallback) error {
	return a.ApplyDeltaChain(ctx, oldMenderPath, []string{deltaPath}, newMenderPath, progressCallback)
}

// ApplyDeltaChain applies multiple deltas: unpack once → apply all → repack once.
func (a *DeltaApplier) ApplyDeltaChain(ctx context.Context, oldMenderPath string, deltaPaths []string, newMenderPath string, progressCallback DeltaProgressCallback) error {
	if len(deltaPaths) == 0 {
		return fmt.Errorf("no delta paths provided")
	}

	// Verify inputs exist
	if _, err := os.Stat(oldMenderPath); err != nil {
		return fmt.Errorf("old mender file does not exist: %s", oldMenderPath)
	}
	for i, dp := range deltaPaths {
		if _, err := os.Stat(dp); err != nil {
			return fmt.Errorf("delta file %d does not exist: %s", i+1, dp)
		}
	}

	if err := os.MkdirAll(filepath.Dir(newMenderPath), 0755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}

	// Ensure temp dir exists
	os.MkdirAll(defaultTempDir, 0755)

	progress := func(percent int, message string) {
		a.logger.Printf("Delta chain: %d%% - %s", percent, message)
		if progressCallback != nil {
			progressCallback(percent)
		}
	}

	if err := a.applier.ApplyChain(ctx, oldMenderPath, deltaPaths, newMenderPath, progress); err != nil {
		return fmt.Errorf("apply delta chain: %w", err)
	}

	// Log result
	if info, err := os.Stat(newMenderPath); err == nil {
		a.logger.Printf("Delta chain complete: %s (%d bytes)", filepath.Base(newMenderPath), info.Size())
	}

	return nil
}

// CleanupDeltaFile removes a delta file after successful application
func (a *DeltaApplier) CleanupDeltaFile(deltaPath string) error {
	a.logger.Printf("Cleaning up delta file: %s", deltaPath)
	if err := os.Remove(deltaPath); err != nil {
		a.logger.Printf("Warning: failed to remove delta file %s: %v", deltaPath, err)
		return err
	}
	return nil
}
