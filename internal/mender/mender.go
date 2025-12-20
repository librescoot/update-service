package mender

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// Manager combines download and installation functionality for Mender updates
type Manager struct {
	downloader    *Downloader
	installer     *Installer
	deltaApplier  *DeltaApplier
	logger        *log.Logger
}

// NewManager creates a new Mender manager with the specified download directory
func NewManager(downloadDir string, logger *log.Logger) *Manager {
	return &Manager{
		downloader:   NewDownloader(downloadDir, logger),
		installer:    NewInstaller(logger),
		deltaApplier: NewDeltaApplier(logger),
		logger:       logger,
	}
}

// DownloadAndVerify downloads an update file and verifies its checksum
func (m *Manager) DownloadAndVerify(ctx context.Context, url, checksum string, progressCallback ProgressCallback) (string, error) {
	m.logger.Printf("Starting download and verification for %s", url)

	// Download the file
	filePath, err := m.downloader.Download(ctx, url, progressCallback)
	if err != nil {
		return "", err
	}

	// Verify checksum if provided
	if checksum != "" {
		if err := m.downloader.VerifyChecksum(filePath, checksum); err != nil {
			return "", err
		}
	}

	// Clean up old downloaded files after successful verification
	if err := m.cleanupOldFiles(url); err != nil {
		m.logger.Printf("Warning: failed to cleanup old files: %v", err)
	}

	return filePath, nil
}

// Install installs the update from the given file path
func (m *Manager) Install(filePath string) error {
	return m.installer.Install(filePath)
}

// Commit commits the installed update
func (m *Manager) Commit() error {
	return m.installer.Commit()
}

// NeedsCommit checks if there's a pending update that needs to be committed
func (m *Manager) NeedsCommit() (bool, error) {
	return m.installer.NeedsCommit()
}

// GetDownloadDir returns the download directory path
func (m *Manager) GetDownloadDir() string {
	return m.downloader.downloadDir
}

// CleanupStaleTmpFiles removes stale .tmp files that don't match the current filename
func (m *Manager) CleanupStaleTmpFiles(currentFilename string) error {
	return m.downloader.CleanupStaleTmpFiles(currentFilename)
}

// cleanupOldFiles removes old downloaded .mender files except the one we're about to download
func (m *Manager) cleanupOldFiles(currentURL string) error {
	currentFilename := filepath.Base(currentURL)
	if currentFilename == "" || currentFilename == "." {
		currentFilename = "update.mender"
	}

	pattern := filepath.Join(m.downloader.downloadDir, "*.mender")
	files, err := filepath.Glob(pattern)
	if err != nil {
		return err
	}

	for _, file := range files {
		if filepath.Base(file) != currentFilename {
			m.logger.Printf("Removing old download file: %s", file)
			if err := os.Remove(file); err != nil {
				m.logger.Printf("Warning: failed to remove old file %s: %v", file, err)
			}
		}
	}

	return nil
}

// CleanupFile removes a downloaded file
func (m *Manager) CleanupFile(filePath string) error {
	// Only clean up files within our download directory
	if !filepath.HasPrefix(filePath, m.downloader.downloadDir) {
		m.logger.Printf("Warning: not cleaning up file outside download directory: %s", filePath)
		return nil
	}

	m.logger.Printf("Cleaning up downloaded file: %s", filePath)
	return filepath.Walk(filePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Remove(path)
	})
}

// RemoveFile removes a specific file (used for cleaning up corrupted downloads)
func (m *Manager) RemoveFile(filePath string) error {
	// Only remove files within our download directory
	if !filepath.HasPrefix(filePath, m.downloader.downloadDir) {
		return fmt.Errorf("refusing to remove file outside download directory: %s", filePath)
	}

	m.logger.Printf("Removing file: %s", filePath)
	if err := os.Remove(filePath); err != nil {
		return fmt.Errorf("failed to remove file %s: %w", filePath, err)
	}
	return nil
}

// FindMenderFileForVersion checks if a .mender file exists for the specified version
// Returns the full path to the file and whether it exists
func (m *Manager) FindMenderFileForVersion(version string) (string, bool) {
	// Get all .mender files in the download directory
	pattern := filepath.Join(m.downloader.downloadDir, "*.mender")
	files, err := filepath.Glob(pattern)
	if err != nil {
		m.logger.Printf("Error searching for mender files: %v", err)
		return "", false
	}

	// Look for a file containing the version string (case-insensitive)
	versionLower := strings.ToLower(version)
	for _, file := range files {
		filenameLower := strings.ToLower(filepath.Base(file))
		if strings.Contains(filenameLower, versionLower) {
			// Verify the file actually exists and is readable
			if _, err := os.Stat(file); err != nil {
				m.logger.Printf("Mender file %s exists in glob but cannot be accessed: %v", file, err)
				continue
			}
			m.logger.Printf("Found mender file for version %s: %s", version, file)
			return file, true
		}
	}

	return "", false
}

// ApplyDeltaUpdate applies a delta update to generate a new mender file
// Returns the path to the new mender file or an error
func (m *Manager) ApplyDeltaUpdate(ctx context.Context, deltaURL, currentVersion string, downloadProgressCallback ProgressCallback, deltaProgressCallback DeltaProgressCallback) (string, error) {
	// Find the existing mender file for the current version
	oldMenderPath, exists := m.FindMenderFileForVersion(currentVersion)
	if !exists {
		return "", fmt.Errorf("no mender file found for current version %s, cannot apply delta", currentVersion)
	}

	// Download the delta file
	m.logger.Printf("Downloading delta update from %s", deltaURL)
	deltaPath, err := m.downloader.Download(ctx, deltaURL, downloadProgressCallback)
	if err != nil {
		return "", fmt.Errorf("failed to download delta file: %w", err)
	}

	// Generate the new mender filename based on the delta filename
	deltaBaseName := filepath.Base(deltaPath)
	newMenderName := deltaBaseName[:len(deltaBaseName)-6] + ".mender" // Replace .delta with .mender
	newMenderPath := filepath.Join(m.downloader.downloadDir, newMenderName)

	// Apply the delta
	err = m.deltaApplier.ApplyDelta(oldMenderPath, deltaPath, newMenderPath, deltaProgressCallback)
	if err != nil {
		// Clean up the delta file on failure
		m.deltaApplier.CleanupDeltaFile(deltaPath)
		return "", fmt.Errorf("failed to apply delta update: %w", err)
	}

	// Clean up the delta file after successful application
	if err := m.deltaApplier.CleanupDeltaFile(deltaPath); err != nil {
		m.logger.Printf("Warning: failed to cleanup delta file: %v", err)
	}

	// Clean up the old mender file after successful delta application
	m.logger.Printf("Removing old mender file after successful delta application: %s", oldMenderPath)
	if err := os.Remove(oldMenderPath); err != nil {
		m.logger.Printf("Warning: failed to remove old mender file %s: %v", oldMenderPath, err)
	}

	return newMenderPath, nil
}
