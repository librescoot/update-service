package mender

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/librescoot/update-service/internal/version"
)

// deltaSanityEpoch guards the age-based delta reap against an unsynced boot
// clock: until the wall clock is plausibly past this date, nothing age-reaps.
var deltaSanityEpoch = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

// Manager combines download and installation functionality for Mender updates
type Manager struct {
	downloader   *Downloader
	installer    *Installer
	deltaApplier *DeltaApplier
	logger       *log.Logger
	// osReleasePath is defaultOsReleasePath unless a test points it elsewhere.
	osReleasePath string
}

// NewManager creates a new Mender manager with the specified download
// directory. budget is passed through to the Downloader unchanged: it is
// called once per download attempt, not once at construction time.
func NewManager(downloadDir string, budget func() Budget, logger *log.Logger) *Manager {
	return &Manager{
		downloader:   NewDownloader(downloadDir, budget, logger),
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
	if err := m.verifyOrDiscard(filePath, checksum); err != nil {
		return "", err
	}

	// Clean up old downloaded files after successful verification
	if err := m.cleanupOldFiles(url); err != nil {
		m.logger.Printf("Warning: failed to cleanup old files: %v", err)
	}

	return filePath, nil
}

// Install installs the update from the given file path.
// If progressCb is non-nil, it receives progress updates (0-100) parsed from mender-update stderr.
func (m *Manager) Install(filePath string, progressCb InstallProgressCallback) error {
	return m.installer.Install(filePath, progressCb)
}

// VerifyChecksum verifies a local file against a "sha256:<hex>" or "<hex>"
// checksum string. Used for local-file installs, which don't go through
// DownloadAndVerify.
func (m *Manager) VerifyChecksum(filePath, checksum string) error {
	return m.downloader.VerifyChecksum(filePath, checksum)
}

// verifyOrDiscard verifies a just-downloaded file and removes it if it does not
// match. An empty checksum skips verification.
//
// Removing the file is the point: Download short-circuits on a size match, so a
// bad file left in the download dir turns every subsequent attempt into a
// re-verification of the same bytes. The retry budget then expires without a
// single byte having been re-fetched, and the caller falls back to a full
// artifact to recover from what a few hundred KB would have fixed.
func (m *Manager) verifyOrDiscard(filePath, checksum string) error {
	if checksum == "" {
		return nil
	}
	err := m.downloader.VerifyChecksum(filePath, checksum)
	if err == nil {
		return nil
	}
	m.logger.Printf("Removing %s so the next attempt re-downloads it", filepath.Base(filePath))
	if rmErr := m.RemoveFile(filePath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		m.logger.Printf("Warning: failed to remove unverified file: %v", rmErr)
	}
	return err
}

// Commit commits the installed update
func (m *Manager) Commit() error {
	return m.installer.Commit()
}

// CommitWithResult commits the installed update and returns detailed result info
func (m *Manager) CommitWithResult() CommitResult {
	return m.installer.CommitWithResult()
}

// Rollback rolls back a pending mender update, clearing the standalone-state from LMDB.
func (m *Manager) Rollback() error {
	return m.installer.Rollback()
}

// GetCurrentArtifact returns the currently committed artifact name
func (m *Manager) GetCurrentArtifact() (string, error) {
	return m.installer.GetCurrentArtifact()
}

// CheckUpdateState checks the current mender update state relative to expected version
func (m *Manager) CheckUpdateState(expectedVersion string) (UpdateState, error) {
	return m.installer.CheckUpdateState(expectedVersion)
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

// artifactNamePrefix is what mender puts in front of the version in the
// artifact name reported by "mender-update show-artifact".
const artifactNamePrefix = "release-"

// CleanupStaleMenderFiles removes all but one .mender file from the download
// directory. Intended to be called on startup to reclaim disk space from old
// downloads.
func (m *Manager) CleanupStaleMenderFiles() {
	m.cleanupStaleMenderFiles(m.runningVersion())
}

// defaultOsReleasePath is the os-release of the running rootfs. Overridable so
// the lookup can be exercised against a fixture.
const defaultOsReleasePath = "/etc/os-release"

// runningVersion returns the version token of the running image.
//
// os-release is the source of truth here because it is the key the rest of the
// service looks artifacts up BY. performDeltaUpdate and applyDelta both resolve
// the delta base with FindMenderFileForVersion(getCurrentVersion()), and
// getCurrentVersion reads version:<component>[version_id] out of Redis, which
// version-service copies verbatim from /etc/os-release. Reading os-release
// directly gets the same token without depending on Redis being populated,
// which on the DBC means not depending on another board.
//
// "mender-update show-artifact" is deliberately NOT used for this. It reports
// the committed artifact, LMDB key "artifact-name", which is a different fact:
// while an update is installed but not yet committed it legitimately still
// names the PREVIOUS version, and it stays there if the commit fails
// (CheckAndCommitPendingUpdate logs and startup continues regardless). Keying
// the cleanup on it meant keeping the file nothing would ever look up while
// deleting the one that would.
//
// It stays as the fallback for the case os-release cannot be read at all.
func (m *Manager) runningVersion() string {
	osVersion, osErr := m.osReleaseVersion()
	if osErr == nil {
		return osVersion
	}
	m.logger.Printf("Cannot read os-release (%v), falling back to the committed mender artifact", osErr)
	artifact, menderErr := m.GetCurrentArtifact()
	if menderErr != nil {
		m.logger.Printf("Cannot determine running version: %v", menderErr)
		return ""
	}
	return strings.TrimPrefix(artifact, artifactNamePrefix)
}

// osReleaseVersion reads VERSION_ID out of the running rootfs's os-release.
// The value is lowercased by the image build, so every comparison against it
// has to be case-insensitive; the artifact filenames keep the original case.
func (m *Manager) osReleaseVersion() (string, error) {
	path := m.osReleasePath
	if path == "" {
		path = defaultOsReleasePath
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "VERSION_ID=")
		if !ok {
			continue
		}
		version := strings.Trim(strings.TrimSpace(rest), `"'`)
		if version == "" {
			return "", fmt.Errorf("%s has an empty VERSION_ID", path)
		}
		return version, nil
	}
	return "", fmt.Errorf("%s has no VERSION_ID", path)
}

// cleanupStaleMenderFiles keeps the artifacts that something will actually ask
// for, and removes the rest.
//
// Two files are worth keeping, because two are used:
//
//   - the artifact for the running version, which is the delta base.
//     performDeltaUpdate and applyDelta resolve it by exactly this token, and
//     without it applyDelta fails with "no-base-image" and a delta update
//     degrades to a full download.
//   - the newest artifact on the running channel that is NEWER than the running
//     version. That is a target already downloaded but not yet installed, left
//     behind by a power cut or a failed install. Downloader.Download stats the
//     final path, verifies its size against the server and skips the transfer
//     when it is complete, so keeping it turns a resumed update into a zero-byte
//     one instead of re-fetching ~170 MB. Once installed it becomes the next
//     base, so it is never wasted.
//
// Everything else goes. An artifact older than the running version is not
// reachable: there is no backwards delta, and a rollback is a mender partition
// operation that needs no artifact. One on another channel is neither a valid
// base (deltas are published per channel) nor a plausible target.
//
// The keeper is anchored on the running version rather than ranked, because a
// directory can hold artifacts from more than one channel and across channels
// version.Compare has only a lexicographic tiebreak to offer. That puts any
// "v..." tag above any "nightly-..." one on the first byte alone, so ranking a
// mixed directory reaped the artifact for the running nightly and kept a stale
// stable one.
//
// Two fallbacks, in order, for when the running version is unknown or its
// artifact is gone, so that a base is always left behind: the newest on the
// running channel, then the newest of all of them.
func (m *Manager) cleanupStaleMenderFiles(runningVersion string) {
	pattern := filepath.Join(m.downloader.downloadDir, "*.mender")
	files, err := filepath.Glob(pattern)
	if err != nil || len(files) <= 1 {
		return
	}

	keep := make(map[string]string, 2)
	if runningVersion != "" {
		for _, file := range files {
			if strings.EqualFold(version.FromFilename(file), runningVersion) {
				keep[file] = "delta base"
				break
			}
		}
		if staged := newestMenderFile(files, func(ver string) bool {
			return version.SameChannel(ver, runningVersion) && version.Compare(ver, runningVersion) > 0
		}); staged != "" {
			keep[staged] = "staged target"
		}
	}

	if len(keep) == 0 && runningVersion != "" {
		if fallback := newestMenderFile(files, func(ver string) bool {
			return version.SameChannel(ver, runningVersion)
		}); fallback != "" {
			keep[fallback] = "newest on the running channel"
		}
	}
	if len(keep) == 0 {
		if fallback := newestMenderFile(files, nil); fallback != "" {
			keep[fallback] = "newest overall"
		}
	}

	// Iterate files, not the map, so the log order is deterministic.
	for _, file := range files {
		if why, ok := keep[file]; ok {
			m.logger.Printf("Keeping mender file %s (%s; running %q)", filepath.Base(file), why, runningVersion)
			continue
		}
		m.logger.Printf("Removing stale mender file: %s", file)
		if err := os.Remove(file); err != nil {
			m.logger.Printf("Warning: failed to remove stale mender file %s: %v", file, err)
		}
	}
}

// newestMenderFile returns the highest-versioned of the files accept passes, or
// "" if it passes none. Comparison is semver-aware so v0.10.0 sorts above
// v0.7.0 (lex would invert that); it is only meaningful within one channel, so
// accept is where the caller confines it to one.
func newestMenderFile(files []string, accept func(ver string) bool) string {
	var newestFile, newestVersion string
	for _, file := range files {
		ver := version.FromFilename(file)
		if accept != nil && !accept(ver) {
			continue
		}
		if newestFile == "" || version.Compare(ver, newestVersion) > 0 {
			newestVersion, newestFile = ver, file
		}
	}
	return newestFile
}

// CleanupStaleDeltaFiles removes obsolete delta artifacts from the download
// directory. The reference version is the newest LOCAL ".mender" token (a
// download-dir artifact, not necessarily the running OS version). A delta is
// reaped when it is provably superseded on the same channel (target version <=
// reference, clock-independent), or as an age backstop for deltas the version
// test cannot judge: cross-channel orphans, unparseable names, or the case
// where the local base ".mender" is itself stale after a channel switch.
func (m *Manager) CleanupStaleDeltaFiles(maxAge time.Duration) {
	dir := m.downloader.downloadDir

	// Reference: newest local .mender token. Computed here, not assumed to
	// have been set by CleanupStaleMenderFiles (which is not always called first).
	var referenceVersion string
	menderFiles, _ := filepath.Glob(filepath.Join(dir, "*.mender"))
	for _, file := range menderFiles {
		ver := version.FromFilename(file)
		if referenceVersion == "" || version.Compare(ver, referenceVersion) > 0 {
			referenceVersion = ver
		}
	}

	// Include the resumable .delta.tmp partials: both call sites run with no
	// concurrent download writer, so reaping a dead partial promptly is safe.
	var candidates []string
	for _, pattern := range []string{filepath.Join(dir, "*.delta"), filepath.Join(dir, "*.delta.tmp")} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		candidates = append(candidates, matches...)
	}

	now := time.Now()
	for _, file := range candidates {
		reap := false

		dv := version.FromFilename(file)
		if version.SameChannel(dv, referenceVersion) && version.Compare(dv, referenceVersion) <= 0 {
			reap = true
		}

		if !reap {
			if info, err := os.Stat(file); err == nil {
				if now.After(deltaSanityEpoch) && info.ModTime().Before(now) && now.Sub(info.ModTime()) > maxAge {
					reap = true
				}
			}
		}

		if reap {
			m.logger.Printf("Removing stale delta file: %s", file)
			if err := os.Remove(file); err != nil {
				m.logger.Printf("Warning: failed to remove stale delta file %s: %v", file, err)
			}
		}
	}
}

// withinDownloadDir reports whether path is inside the download directory.
//
// It guards two recursive delete paths, so a string prefix test will not do:
// that would accept a sibling directory sharing the name prefix, and would not
// resolve traversal segments. Abs cleans the path lexically and Rel reports
// whether the result escapes.
//
// Symlinks are not resolved. That needs the path to exist, which is not true of
// every caller, and the directory is service-owned.
func (m *Manager) withinDownloadDir(path string) bool {
	absDir, err := filepath.Abs(m.downloader.downloadDir)
	if err != nil {
		return false
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absDir, absPath)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// CleanupFile removes a downloaded file
func (m *Manager) CleanupFile(filePath string) error {
	// Only clean up files within our download directory
	if !m.withinDownloadDir(filePath) {
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
	if !m.withinDownloadDir(filePath) {
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
			m.logger.Printf("Using mender file for %s: %s", version, filepath.Base(file))
			return file, true
		}
	}

	return "", false
}

// FindLatestMenderFile finds the newest .mender file in the download directory for the given channel
// Returns the path, extracted version, and whether a file was found
func (m *Manager) FindLatestMenderFile(channel string) (path string, menderVersion string, found bool) {
	pattern := filepath.Join(m.downloader.downloadDir, "*.mender")
	files, err := filepath.Glob(pattern)
	if err != nil {
		m.logger.Printf("Error searching for mender files: %v", err)
		return "", "", false
	}

	if len(files) == 0 {
		return "", "", false
	}

	// Filter to files matching the channel and find the newest by version.
	// Stable filenames are "...-v1.0.0.mender" with no channel infix, so we
	// classify by the extracted version token rather than substring matching.
	var newestFile string
	var newestVersion string

	for _, file := range files {
		ver := version.FromFilename(file)
		if ver == "" || version.Channel(ver) != channel {
			continue
		}
		if newestFile == "" || version.Compare(ver, newestVersion) > 0 {
			newestVersion = ver
			newestFile = file
		}
	}

	if newestFile == "" {
		return "", "", false
	}

	m.logger.Printf("Found latest mender file: %s (version: %s)", newestFile, newestVersion)
	return newestFile, newestVersion, true
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
	err = m.deltaApplier.ApplyDelta(ctx, oldMenderPath, deltaPath, newMenderPath, deltaProgressCallback)
	if err != nil {
		// Clean up the delta file on failure; CleanupDeltaFile logs its own
		// failures, and the error being returned here is the one that matters.
		_ = m.deltaApplier.CleanupDeltaFile(deltaPath)
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

// DownloadDelta downloads a delta file without applying it
// Returns the path to the downloaded delta file
func (m *Manager) DownloadDelta(ctx context.Context, deltaURL, checksum string, progressCallback ProgressCallback) (string, error) {
	deltaPath, err := m.downloader.Download(ctx, deltaURL, progressCallback)
	if err != nil {
		return "", fmt.Errorf("failed to download delta file: %w", err)
	}
	if err := m.verifyOrDiscard(deltaPath, checksum); err != nil {
		return "", fmt.Errorf("delta checksum verification failed: %w", err)
	}
	return deltaPath, nil
}

// ApplyDownloadedDelta applies a pre-downloaded delta file to generate a new mender file
// Returns the path to the new mender file or an error
func (m *Manager) ApplyDownloadedDelta(ctx context.Context, deltaPath, currentVersion string, deltaProgressCallback DeltaProgressCallback) (string, error) {
	// Find the existing mender file for the current version
	oldMenderPath, exists := m.FindMenderFileForVersion(currentVersion)
	if !exists {
		return "", fmt.Errorf("no mender file found for current version %s, cannot apply delta", currentVersion)
	}

	// Generate the new mender filename based on the delta filename
	deltaBaseName := filepath.Base(deltaPath)
	newMenderName := deltaBaseName[:len(deltaBaseName)-6] + ".mender" // Replace .delta with .mender
	newMenderPath := filepath.Join(m.downloader.downloadDir, newMenderName)

	err := m.deltaApplier.ApplyDelta(ctx, oldMenderPath, deltaPath, newMenderPath, deltaProgressCallback)
	if err != nil {
		if ctx.Err() == nil {
			// CleanupDeltaFile logs its own failures; the error being
			// returned here is the one that matters.
			_ = m.deltaApplier.CleanupDeltaFile(deltaPath)
		}
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

// ApplyDownloadedDeltaChain applies multiple pre-downloaded deltas in a single
// unpack/repack cycle, avoiding intermediate compress+repack overhead.
// Returns the path to the final mender file.
func (m *Manager) ApplyDownloadedDeltaChain(ctx context.Context, deltaPaths []string, deltaVersions []string, currentVersion string, deltaProgressCallback DeltaProgressCallback) (string, error) {
	if len(deltaPaths) == 0 {
		return "", fmt.Errorf("no delta paths provided")
	}
	if len(deltaPaths) != len(deltaVersions) {
		return "", fmt.Errorf("deltaPaths and deltaVersions length mismatch")
	}

	// Single delta: use existing method
	if len(deltaPaths) == 1 {
		return m.ApplyDownloadedDelta(ctx, deltaPaths[0], currentVersion, deltaProgressCallback)
	}

	// Find the base mender file
	oldMenderPath, exists := m.FindMenderFileForVersion(currentVersion)
	if !exists {
		return "", fmt.Errorf("no mender file found for current version %s, cannot apply delta chain", currentVersion)
	}

	// Output is named after the final delta
	finalDeltaBase := filepath.Base(deltaPaths[len(deltaPaths)-1])
	newMenderName := finalDeltaBase[:len(finalDeltaBase)-6] + ".mender"
	newMenderPath := filepath.Join(m.downloader.downloadDir, newMenderName)

	err := m.deltaApplier.ApplyDeltaChain(ctx, oldMenderPath, deltaPaths, newMenderPath, deltaProgressCallback)
	if err != nil {
		// Don't clean up delta files on context cancellation — they're
		// valid downloads that can be reused on the next attempt.
		if ctx.Err() == nil {
			for _, dp := range deltaPaths {
				// CleanupDeltaFile logs its own failures; the error being
				// returned below is the one that matters.
				_ = m.deltaApplier.CleanupDeltaFile(dp)
			}
		}
		return "", fmt.Errorf("failed to apply delta chain: %w", err)
	}

	// Clean up all delta files
	for _, dp := range deltaPaths {
		if err := m.deltaApplier.CleanupDeltaFile(dp); err != nil {
			m.logger.Printf("Warning: failed to cleanup delta file: %v", err)
		}
	}

	// Remove old mender file
	m.logger.Printf("Removing old mender file after successful chain application: %s", oldMenderPath)
	if err := os.Remove(oldMenderPath); err != nil {
		m.logger.Printf("Warning: failed to remove old mender file %s: %v", oldMenderPath, err)
	}

	return newMenderPath, nil
}

// CleanupDeltaFile removes a delta file (used when downloads are cancelled or fail)
func (m *Manager) CleanupDeltaFile(deltaPath string) error {
	return m.deltaApplier.CleanupDeltaFile(deltaPath)
}
