package mender

import (
	"context"
	"log"
	"os"
	"path/filepath"
)

// Manager combines download and installation functionality for Mender updates
type Manager struct {
	downloader *Downloader
	installer  *Installer
	logger     *log.Logger
}

// NewManager creates a new Mender manager with the specified download directory
func NewManager(downloadDir string, logger *log.Logger) *Manager {
	return &Manager{
		downloader: NewDownloader(downloadDir, logger),
		installer:  NewInstaller(logger),
		logger:     logger,
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
