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
	StateCommitted                // The expected artifact is active and committed.
	StateNeedsReboot              // Install succeeded in the inactive partition.
	StateNeedsCommit              // Commit-enter state; running rootfs determines whether reboot happened.
	StateNeedsResume              // Interrupted Mender transition must be resumed.
	StateInconsistent             // Mender marked the artifact failed; do not continue normally.
)

// UpdateObservation separates the durable facts Mender records. In particular,
// CommittedArtifact and PendingArtifact legitimately name different releases
// between install and commit.
type UpdateObservation struct {
	State             UpdateState
	CommittedArtifact string
	CommittedVersion  string
	PendingArtifact   string
	PendingVersion    string
	MenderState       string
}

// VersionFromArtifact converts this project's Mender artifact names to the
// VERSION_ID written into /etc/os-release. Keep the raw artifact beside this
// value whenever exact identity matters.
func VersionFromArtifact(artifact string) string {
	artifact = strings.TrimSuffix(artifact, "_INCONSISTENT")
	artifact = strings.TrimPrefix(artifact, "release-")
	return strings.TrimSuffix(artifact, "-minimal")
}

type Installer struct {
	logger *log.Logger

	// menderConfPaths, deviceSize and readStatus let tests drive hardware and
	// Mender observations. Nil means the production defaults.
	menderConfPaths []string
	deviceSize      func(string) (int64, error)
	readStatus      func() (*menderstatus.Status, error)
}

func NewInstaller(logger *log.Logger) *Installer {
	return &Installer{
		logger: logger,
	}
}

// Resume asks Mender to continue an interrupted standalone transition.
func (i *Installer) Resume() error {
	i.logger.Printf("Resuming mender update")
	cmd := exec.Command("mender-update", "resume")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mender-update resume failed: %w, stderr: %s", err, stderr.String())
	}
	i.logger.Printf("mender-update resume output: %s", stdout.String())
	return nil
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

func readDefaultStatus() (*menderstatus.Status, error) {
	reader, err := menderstatus.NewReaderDefault()
	if err != nil {
		return nil, fmt.Errorf("failed to create mender status reader: %w", err)
	}
	status, err := reader.ReadStatus()
	if err != nil {
		return nil, fmt.Errorf("failed to read mender status: %w", err)
	}
	return status, nil
}

// ObserveUpdate reads Mender's durable committed and standalone state. It does
// not consult Redis: Redis is deliberately volatile and can be reconstructed
// from this observation after a reboot.
func (i *Installer) ObserveUpdate() (UpdateObservation, error) {
	readStatus := i.readStatus
	if readStatus == nil {
		readStatus = readDefaultStatus
	}
	status, err := readStatus()
	if err != nil {
		return UpdateObservation{}, err
	}
	return observeStatus(status), nil
}

func observeStatus(status *menderstatus.Status) UpdateObservation {
	observation := UpdateObservation{
		State:             StateNoUpdate,
		CommittedArtifact: status.CommittedArtifact,
		CommittedVersion:  VersionFromArtifact(status.CommittedArtifact),
	}

	if strings.HasSuffix(status.CommittedArtifact, "_INCONSISTENT") {
		observation.State = StateInconsistent
		return observation
	}
	if !status.UpdateInProgress || status.State == nil {
		return observation
	}

	observation.PendingArtifact = status.State.ArtifactName
	observation.PendingVersion = VersionFromArtifact(status.State.ArtifactName)
	observation.MenderState = status.State.InState

	switch {
	case status.NeedsCommit() && !status.State.Failed:
		observation.State = StateNeedsCommit
	default:
		// Cleanup, rollback, failure handling, and interrupted install states
		// are all advanced by `mender-update resume`. Failed is not terminal
		// while standalone-state still exists; resume removes it or records the
		// final committed artifact (including _INCONSISTENT when unrecoverable).
		observation.State = StateNeedsResume
	}
	return observation
}

// CheckUpdateState remains as a compatibility wrapper for callers that only
// need the coarse state. expectedVersion is intentionally ignored; Mender's
// LMDB is authoritative for pending-update state.
func (i *Installer) CheckUpdateState(expectedVersion string) (UpdateState, error) {
	observation, err := i.ObserveUpdate()
	if err != nil {
		return StateNoUpdate, err
	}
	if observation.State == StateNoUpdate && expectedVersion != "" &&
		observation.CommittedVersion == VersionFromArtifact(expectedVersion) {
		return StateCommitted, nil
	}
	return observation.State, nil
}
