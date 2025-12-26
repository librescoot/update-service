package mender

import (
	"bytes"
	"fmt"
	"log"
	"os/exec"
	"strings"

	menderstatus "github.com/librescoot/librescoot-mender-status/mender"
)

// UpdateState represents the state of a mender update
type UpdateState int

const (
	StateNoUpdate     UpdateState = iota // No update in progress
	StateCommitted                       // Update successfully committed
	StateNeedsReboot                     // Update installed, waiting for reboot
	StateInconsistent                    // Update failed, system inconsistent
)

// Installer handles Mender update installation and commit operations
type Installer struct {
	logger *log.Logger
}

// NewInstaller creates a new installer instance
func NewInstaller(logger *log.Logger) *Installer {
	return &Installer{
		logger: logger,
	}
}

// NeedsCommit checks if there's a pending update that needs to be committed
func (i *Installer) NeedsCommit() (bool, error) {
	// Always try to commit on startup - if there's nothing to commit, mender will handle it gracefully
	return true, nil
}

// Install installs the update from the given file path
func (i *Installer) Install(filePath string) error {
	i.logger.Printf("Installing update from %s", filePath)
	cmd := exec.Command("mender-update", "install", filePath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("error running mender-update install: %w, stderr: %s", err, stderr.String())
	}

	i.logger.Printf("mender-update install output: %s", stdout.String())
	return nil
}

// CommitResult represents the result of a mender commit operation
type CommitResult struct {
	Success  bool   // True if commit succeeded
	ExitCode int    // Exit code from mender-update
	Output   string // stdout from command
	Error    string // stderr from command
}

// Commit commits the installed update
func (i *Installer) Commit() error {
	result := i.CommitWithResult()
	if !result.Success {
		return fmt.Errorf("mender-update commit failed (exit %d): %s", result.ExitCode, result.Error)
	}
	return nil
}

// CommitWithResult commits the installed update and returns detailed result info
func (i *Installer) CommitWithResult() CommitResult {
	cmd := exec.Command("mender-update", "commit")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		i.logger.Printf("mender-update commit output: %s", stdout.String())
		return CommitResult{
			Success:  true,
			ExitCode: 0,
			Output:   stdout.String(),
		}
	}

	// Get exit code
	exitCode := 1 // default
	if exitErr, ok := err.(*exec.ExitError); ok {
		exitCode = exitErr.ExitCode()
	}

	return CommitResult{
		Success:  false,
		ExitCode: exitCode,
		Output:   stdout.String(),
		Error:    stderr.String(),
	}
}

// GetCurrentArtifact returns the currently committed artifact name
func (i *Installer) GetCurrentArtifact() (string, error) {
	cmd := exec.Command("mender-update", "show-artifact")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("mender-update show-artifact failed: %w, stderr: %s", err, stderr.String())
	}

	return strings.TrimSpace(stdout.String()), nil
}

// CheckUpdateState checks the current mender update state relative to expected version.
// Uses the mender-status library to read the standalone-state from Mender's LMDB database.
func (i *Installer) CheckUpdateState(expectedVersion string) (UpdateState, error) {
	// Read mender status from LMDB database
	reader, err := menderstatus.NewReaderDefault()
	if err != nil {
		return StateNoUpdate, fmt.Errorf("failed to create mender status reader: %w", err)
	}

	status, err := reader.ReadStatus()
	if err != nil {
		return StateNoUpdate, fmt.Errorf("failed to read mender status: %w", err)
	}

	// Get committed artifact name
	committedArtifact := status.CommittedArtifact

	// Check for inconsistent state (failed update)
	if strings.HasSuffix(committedArtifact, "_INCONSISTENT") {
		i.logger.Printf("Mender: INCONSISTENT state (%s)", committedArtifact)
		return StateInconsistent, nil
	}

	// Check if update is in progress
	if status.UpdateInProgress {
		if status.State.Failed {
			i.logger.Printf("Mender: update failed (state=%s)", status.State.InState)
			return StateInconsistent, nil
		}
		if status.NeedsCommit() {
			i.logger.Printf("Mender: pending commit for %s", status.State.ArtifactName)
			return StateNeedsReboot, nil
		}
		i.logger.Printf("Mender: update in progress (state=%s)", status.State.InState)
		return StateNoUpdate, nil
	}

	// No update in progress
	if expectedVersion != "" && committedArtifact == expectedVersion {
		i.logger.Printf("Mender: running %s (expected)", committedArtifact)
		return StateCommitted, nil
	}

	i.logger.Printf("Mender: running %s, no update pending", committedArtifact)
	return StateNoUpdate, nil
}
