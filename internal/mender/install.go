package mender

import (
	"bytes"
	"fmt"
	"log"
	"os/exec"
	"strings"
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
// Returns the state and attempts commit if appropriate.
func (i *Installer) CheckUpdateState(expectedVersion string) (UpdateState, error) {
	current, err := i.GetCurrentArtifact()
	if err != nil {
		return StateNoUpdate, err
	}

	i.logger.Printf("Current artifact: %s, expected: %s", current, expectedVersion)

	// Check for inconsistent state (failed update)
	if strings.HasSuffix(current, "_INCONSISTENT") {
		i.logger.Printf("System in INCONSISTENT state: %s", current)
		return StateInconsistent, nil
	}

	// Check if already committed to expected version
	if current == expectedVersion {
		i.logger.Printf("Already running expected version")
		return StateCommitted, nil
	}

	// Try to commit - this tells us what state we're in
	commitCmd := exec.Command("mender-update", "commit")
	var commitStderr bytes.Buffer
	commitCmd.Stderr = &commitStderr

	commitErr := commitCmd.Run()
	if commitErr == nil {
		// Commit succeeded - verify we're now on expected version
		newCurrent, _ := i.GetCurrentArtifact()
		if newCurrent == expectedVersion {
			i.logger.Printf("Commit succeeded, now running %s", newCurrent)
			return StateCommitted, nil
		}
		i.logger.Printf("Commit succeeded but artifact is %s (expected %s)", newCurrent, expectedVersion)
		return StateCommitted, nil
	}

	// Check exit code
	if exitErr, ok := commitErr.(*exec.ExitError); ok {
		exitCode := exitErr.ExitCode()
		switch exitCode {
		case 2:
			// No update in progress
			i.logger.Printf("No update in progress (current: %s)", current)
			return StateNoUpdate, nil
		case 1:
			// Update waiting - either needs reboot or sanity check failed
			i.logger.Printf("Update waiting (exit 1): %s", commitStderr.String())
			return StateNeedsReboot, nil
		default:
			i.logger.Printf("Commit failed (exit %d): %s", exitCode, commitStderr.String())
			return StateNeedsReboot, nil
		}
	}

	return StateNoUpdate, fmt.Errorf("failed to run mender-update commit: %w", commitErr)
}
