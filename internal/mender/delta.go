package mender

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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

// DeltaProgressCallback is called with progress updates during delta application
// percent: 0-100 indicating progress
type DeltaProgressCallback func(percent int)

// ApplyDelta applies a delta update to an old mender file to generate a new one
// oldMenderPath: path to the existing .mender file
// deltaPath: path to the downloaded .delta file
// newMenderPath: path where the new .mender file should be created
// progressCallback: optional callback for progress updates (can be nil)
func (a *DeltaApplier) ApplyDelta(ctx context.Context, oldMenderPath, deltaPath, newMenderPath string, progressCallback DeltaProgressCallback) error {
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
	// Use a 30-minute timeout to prevent indefinite hangs
	deltaCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(deltaCtx, "nice", "-n", "10", "/usr/bin/mender-apply-delta.py", oldMenderPath, deltaPath, newMenderPath)

	// Set up pipes to capture and parse stdout/stderr
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	a.logger.Printf("Applying delta update: %s + %s -> %s",
		filepath.Base(oldMenderPath),
		filepath.Base(deltaPath),
		filepath.Base(newMenderPath))

	// Start the command
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start delta application: %w", err)
	}

	// Capture all output for logging
	var stdoutBuf, stderrBuf bytes.Buffer

	// Parse stdout for progress updates in a goroutine
	stdoutDone := make(chan struct{})
	go func() {
		defer close(stdoutDone)
		scanner := bufio.NewScanner(stdoutPipe)
		for scanner.Scan() {
			line := scanner.Text()
			stdoutBuf.WriteString(line + "\n")

			// Parse progress lines: PROGRESS:percent:message
			if strings.HasPrefix(line, "PROGRESS:") {
				parts := strings.SplitN(line, ":", 3)
				if len(parts) >= 2 {
					if percent, err := strconv.Atoi(parts[1]); err == nil {
						if len(parts) == 3 {
							a.logger.Printf("Delta progress: %d%% - %s", percent, parts[2])
						} else {
							a.logger.Printf("Delta progress: %d%%", percent)
						}
						if progressCallback != nil {
							progressCallback(percent)
						}
					}
				}
			}
		}
	}()

	// Capture stderr
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		io.Copy(&stderrBuf, stderrPipe)
	}()

	// Wait for output capture to complete
	<-stdoutDone
	<-stderrDone

	// Wait for command to complete
	err = cmd.Wait()
	if err != nil {
		a.logger.Printf("Delta application failed - stdout: %s", stdoutBuf.String())
		a.logger.Printf("Delta application failed - stderr: %s", stderrBuf.String())
		return fmt.Errorf("failed to apply delta update: %w, stderr: %s", err, stderrBuf.String())
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

	if stdoutBuf.String() != "" {
		a.logger.Printf("Delta application output: %s", stdoutBuf.String())
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