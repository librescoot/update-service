package mender

import (
	"bufio"
	"bytes"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"strings"

	menderstatus "github.com/librescoot/librescoot-mender-status/mender"
)

type InstallProgressCallback func(percent int)

type UpdateState int

const (
	StateNoUpdate     UpdateState = iota
	StateCommitted                // Expected artifact is active and committed.
	StateNeedsReboot              // Install succeeded in the inactive partition.
	StateNeedsCommit              // Booted into the new partition; Mender must commit it.
	StateInconsistent             // Mender marked the artifact failed; do not continue normally.
)

type Installer struct {
	logger *log.Logger

	// menderConfPaths and deviceSize let tests drive the fit check against a
	// temp dir. Nil means the production defaults.
	menderConfPaths []string
	deviceSize      func(string) (int64, error)
}

func NewInstaller(logger *log.Logger) *Installer {
	return &Installer{
		logger: logger,
	}
}

// Rollback asks Mender to discard the pending standalone update state.
func (i *Installer) Rollback() error {
	i.logger.Printf("Rolling back mender update")
	cmd := exec.Command("mender-update", "rollback")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mender-update rollback failed: %w, stderr: %s", err, stderr.String())
	}

	i.logger.Printf("mender-update rollback output: %s", stdout.String())
	return nil
}

// Install installs the update from the given file path.
// If progressCb is non-nil, it receives progress updates (0-100) parsed from
// mender-update's stderr output (format: "\r<percent>%").
//
// An artifact whose rootfs payload is larger than the target slot is refused
// with ErrArtifactTooLarge before mender-update runs, so nothing is written.
func (i *Installer) Install(filePath string, progressCb InstallProgressCallback) error {
	i.logger.Printf("Installing update from %s", filePath)

	if err := i.checkArtifactFits(filePath); err != nil {
		return err
	}

	cmd := exec.Command("mender-update", "install", filePath)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start mender-update install: %w", err)
	}

	var stderrBuf bytes.Buffer
	scanner := bufio.NewScanner(stderrPipe)
	scanner.Split(func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		for i := range data {
			if data[i] == '\r' || data[i] == '\n' {
				return i + 1, data[:i], nil
			}
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		if before, ok := strings.CutSuffix(line, "%"); ok {
			numStr := before
			if pct, err := strconv.Atoi(numStr); err == nil && pct >= 0 && pct <= 100 {
				if progressCb != nil {
					progressCb(pct)
				}
				continue
			}
		}

		stderrBuf.WriteString(line)
		stderrBuf.WriteByte('\n')
	}

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("error running mender-update install: %w, stderr: %s", err, stderrBuf.String())
	}

	i.logger.Printf("mender-update install output: %s", stdout.String())
	return nil
}

type CommitResult struct {
	Success  bool
	ExitCode int
	Output   string
	Error    string
}

func (i *Installer) Commit() error {
	result := i.CommitWithResult()
	if !result.Success {
		return fmt.Errorf("mender-update commit failed (exit %d): %s", result.ExitCode, result.Error)
	}
	return nil
}

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

	exitCode := 1
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

// CheckUpdateState interprets Mender's LMDB standalone state relative to expectedVersion.
func (i *Installer) CheckUpdateState(expectedVersion string) (UpdateState, error) {
	reader, err := menderstatus.NewReaderDefault()
	if err != nil {
		return StateNoUpdate, fmt.Errorf("failed to create mender status reader: %w", err)
	}

	status, err := reader.ReadStatus()
	if err != nil {
		return StateNoUpdate, fmt.Errorf("failed to read mender status: %w", err)
	}

	committedArtifact := status.CommittedArtifact

	if strings.HasSuffix(committedArtifact, "_INCONSISTENT") {
		i.logger.Printf("Mender: INCONSISTENT state (%s)", committedArtifact)
		return StateInconsistent, nil
	}

	if status.UpdateInProgress {
		if status.State.Failed {
			i.logger.Printf("Mender: update failed (state=%s)", status.State.InState)
			return StateInconsistent, nil
		}
		if status.NeedsCommit() {
			i.logger.Printf("Mender: pending commit for %s", status.State.ArtifactName)
			return StateNeedsCommit, nil
		}
		i.logger.Printf("Mender: reboot pending for %s (state=%s)", status.State.ArtifactName, status.State.InState)
		return StateNeedsReboot, nil
	}

	if expectedVersion != "" && committedArtifact == expectedVersion {
		i.logger.Printf("Mender: running %s (expected)", committedArtifact)
		return StateCommitted, nil
	}

	i.logger.Printf("Mender: running %s, no update pending", committedArtifact)
	return StateNoUpdate, nil
}
