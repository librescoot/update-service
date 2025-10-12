package mender

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
)

// DeltaApplier handles applying delta updates to generate new mender files
type DeltaApplier struct {
	logger *log.Logger
}

// NewDeltaApplier creates a new delta applier instance
func NewDeltaApplier(logger *log.Logger) *DeltaApplier {
	return &DeltaApplier{
		logger: logger,
	}
}

// ApplyDelta applies a delta update to an old mender file to generate a new one
// oldMenderPath: path to the existing .mender file
// deltaPath: path to the downloaded .delta file
// newMenderPath: path where the new .mender file should be created
func (a *DeltaApplier) ApplyDelta(oldMenderPath, deltaPath, newMenderPath string) error {
	// Verify input files exist
	if _, err := os.Stat(oldMenderPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("old mender file does not exist: %s", oldMenderPath)
		}
		return fmt.Errorf("error checking old mender file: %w", err)
	}

	if _, err := os.Stat(deltaPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("delta file does not exist: %s", deltaPath)
		}
		return fmt.Errorf("error checking delta file: %w", err)
	}

	// Ensure the output directory exists
	outputDir := filepath.Dir(newMenderPath)
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	// Execute the delta application script with nice priority (lower CPU priority)
	cmd := exec.Command("nice", "-n", "10", "/usr/bin/mender-apply-delta.py", oldMenderPath, deltaPath, newMenderPath)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	a.logger.Printf("Applying delta update: %s + %s -> %s",
		filepath.Base(oldMenderPath),
		filepath.Base(deltaPath),
		filepath.Base(newMenderPath))

	err := cmd.Run()
	if err != nil {
		a.logger.Printf("Delta application failed - stdout: %s", stdout.String())
		a.logger.Printf("Delta application failed - stderr: %s", stderr.String())
		return fmt.Errorf("failed to apply delta update: %w, stderr: %s", err, stderr.String())
	}

	// Verify the new file was created
	if _, err := os.Stat(newMenderPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("delta application succeeded but new mender file was not created: %s", newMenderPath)
		}
		return fmt.Errorf("error checking new mender file: %w", err)
	}

	// Log file sizes for monitoring
	oldInfo, _ := os.Stat(oldMenderPath)
	deltaInfo, _ := os.Stat(deltaPath)
	newInfo, _ := os.Stat(newMenderPath)

	a.logger.Printf("Delta application successful - old: %d bytes, delta: %d bytes, new: %d bytes",
		oldInfo.Size(), deltaInfo.Size(), newInfo.Size())

	if stdout.String() != "" {
		a.logger.Printf("Delta application output: %s", stdout.String())
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