package mender

import (
	"bytes"
	"fmt"
	"log"
	"os/exec"
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

// Commit commits the installed update
func (i *Installer) Commit() error {
	i.logger.Printf("Committing update")
	cmd := exec.Command("mender-update", "commit")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("error running mender-update commit: %w, stderr: %s", err, stderr.String())
	}

	i.logger.Printf("mender-update commit output: %s", stdout.String())
	return nil
}