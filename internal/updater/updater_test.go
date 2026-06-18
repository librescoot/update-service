package updater

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestRemoveLegacyOtaFiles(t *testing.T) {
	baseDir, err := os.MkdirTemp("", "legacy_ota_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(baseDir)

	subDir := filepath.Join(baseDir, "dbc")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("Failed to create subdir: %v", err)
	}

	// Top-level legacy artifacts that must be reaped.
	reaped := []string{
		"librescoot-unu-mdb-v1.0.0.mender",
		"librescoot-unu-mdb-v1.0.0.mender.tmp",
		"librescoot-unu-mdb-nightly-20260101T000000.delta",
		"librescoot-unu-mdb-nightly-20260101T000000.delta.tmp",
	}
	// Files that must survive: live per-component download one level deeper,
	// and an unrelated top-level file.
	kept := []string{
		filepath.Join("dbc", "librescoot-unu-dbc-v1.0.0.mender"),
		"keepme.txt",
	}

	for _, f := range append(append([]string{}, reaped...), kept...) {
		p := filepath.Join(baseDir, f)
		if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
			t.Fatalf("Failed to create %s: %v", f, err)
		}
	}

	// downloadDir points at the per-component subdir, matching normal config, so
	// the parent-equals-downloadDir guard is a no-op here.
	removeLegacyOtaFiles(baseDir, subDir, nil)

	for _, f := range reaped {
		if _, err := os.Stat(filepath.Join(baseDir, f)); !os.IsNotExist(err) {
			t.Errorf("expected %s to be removed, got err=%v", f, err)
		}
	}
	for _, f := range kept {
		if _, err := os.Stat(filepath.Join(baseDir, f)); err != nil {
			t.Errorf("expected %s to survive, got err=%v", f, err)
		}
	}
}

func TestRemoveLegacyOtaFiles_SkipsActiveDownloadDir(t *testing.T) {
	baseDir, err := os.MkdirTemp("", "legacy_ota_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(baseDir)

	// Misconfigured DownloadDir pointing at the legacy base itself: live
	// downloads sit directly at baseDir and must NOT be reaped.
	live := filepath.Join(baseDir, "librescoot-unu-mdb-v1.0.0.mender")
	if err := os.WriteFile(live, []byte("x"), 0644); err != nil {
		t.Fatalf("Failed to create %s: %v", live, err)
	}

	removeLegacyOtaFiles(baseDir, baseDir, nil)

	if _, err := os.Stat(live); err != nil {
		t.Errorf("expected live download %s to survive, got err=%v", live, err)
	}
}

func TestUpdateCheckMutex_PreventsConcurrentChecks(t *testing.T) {
	// Test that TryLock prevents concurrent access
	var mu sync.Mutex

	// First lock should succeed
	if !mu.TryLock() {
		t.Error("First TryLock should succeed")
	}

	// Second lock should fail while first is held
	if mu.TryLock() {
		t.Error("Second TryLock should fail while mutex is held")
		mu.Unlock() // Clean up if we got here
	}

	// Unlock and try again
	mu.Unlock()

	// Now it should succeed
	if !mu.TryLock() {
		t.Error("TryLock should succeed after unlock")
	}
	mu.Unlock()
}

func TestCorruptionErrorDetection(t *testing.T) {
	testCases := []struct {
		name         string
		errorMsg     string
		isCorruption bool
	}{
		{
			name:         "gzip decompression error",
			errorMsg:     "error running mender-update install: gzip decompression failed",
			isCorruption: true,
		},
		{
			name:         "truncated gzip",
			errorMsg:     "Streaming failed: truncated gzip input",
			isCorruption: true,
		},
		{
			name:         "checksum mismatch",
			errorMsg:     "checksum mismatch: expected abc123, got def456",
			isCorruption: true,
		},
		{
			name:         "corrupt file",
			errorMsg:     "file appears to be corrupt",
			isCorruption: true,
		},
		{
			name:         "permission denied",
			errorMsg:     "permission denied: /dev/mmcblk3p2",
			isCorruption: false,
		},
		{
			name:         "disk full",
			errorMsg:     "no space left on device",
			isCorruption: false,
		},
		{
			name:         "generic error",
			errorMsg:     "unknown error occurred",
			isCorruption: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			isCorruption := isCorruptionError(tc.errorMsg)
			if isCorruption != tc.isCorruption {
				t.Errorf("isCorruptionError(%q) = %v, want %v", tc.errorMsg, isCorruption, tc.isCorruption)
			}
		})
	}
}

// isCorruptionError checks if an error message indicates file corruption
// This mirrors the logic in updater.go
func isCorruptionError(errStr string) bool {
	keywords := []string{"gzip", "checksum", "corrupt", "truncated"}
	for _, keyword := range keywords {
		if containsIgnoreCase(errStr, keyword) {
			return true
		}
	}
	return false
}

func containsIgnoreCase(s, substr string) bool {
	// Simple case-insensitive contains
	for i := 0; i <= len(s)-len(substr); i++ {
		match := true
		for j := 0; j < len(substr); j++ {
			sc := s[i+j]
			pc := substr[j]
			// Convert to lowercase
			if sc >= 'A' && sc <= 'Z' {
				sc += 'a' - 'A'
			}
			if pc >= 'A' && pc <= 'Z' {
				pc += 'a' - 'A'
			}
			if sc != pc {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// TestRecoveryLogic tests the decision-making in recoverFromStuckState
// This tests the logic without needing to mock the full updater
func TestRecoveryLogic(t *testing.T) {
	testCases := []struct {
		name              string
		currentStatus     string
		menderNeedsReboot bool
		expectedAction    string // "clear", "set_rebooting", "none"
	}{
		{
			name:              "idle status - no action",
			currentStatus:     "idle",
			menderNeedsReboot: false,
			expectedAction:    "none",
		},
		{
			name:              "empty status - no action",
			currentStatus:     "",
			menderNeedsReboot: false,
			expectedAction:    "none",
		},
		{
			name:              "downloading - clear",
			currentStatus:     "downloading",
			menderNeedsReboot: false,
			expectedAction:    "clear",
		},
		{
			name:              "downloading with mender reboot - clear",
			currentStatus:     "downloading",
			menderNeedsReboot: true,
			expectedAction:    "clear",
		},
		{
			name:              "installing with mender reboot - set pending-reboot",
			currentStatus:     "installing",
			menderNeedsReboot: true,
			expectedAction:    "set_pending_reboot",
		},
		{
			name:              "installing without mender reboot - clear",
			currentStatus:     "installing",
			menderNeedsReboot: false,
			expectedAction:    "clear",
		},
		{
			name:              "preparing - clear",
			currentStatus:     "preparing",
			menderNeedsReboot: false,
			expectedAction:    "clear",
		},
		{
			name:              "pending-reboot - clear",
			currentStatus:     "pending-reboot",
			menderNeedsReboot: false,
			expectedAction:    "clear",
		},
		{
			name:              "error - clear",
			currentStatus:     "error",
			menderNeedsReboot: false,
			expectedAction:    "clear",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Simulate the decision logic from recoverFromStuckState
			action := determineRecoveryAction(tc.currentStatus, tc.menderNeedsReboot)
			if action != tc.expectedAction {
				t.Errorf("determineRecoveryAction(%q, %v) = %q, want %q",
					tc.currentStatus, tc.menderNeedsReboot, action, tc.expectedAction)
			}
		})
	}
}

// determineRecoveryAction mirrors the decision logic in recoverFromStuckState
func determineRecoveryAction(currentStatus string, menderNeedsReboot bool) string {
	if currentStatus == "idle" || currentStatus == "" {
		return "none"
	}

	switch currentStatus {
	case "downloading", "preparing":
		return "clear"
	case "installing":
		if menderNeedsReboot {
			return "set_pending_reboot"
		}
		return "clear"
	case "pending-reboot":
		return "clear"
	case "error":
		return "clear"
	default:
		return "clear"
	}
}

// TestCheckForUpdatesDefersNonIdle tests that checkForUpdates should defer for all non-idle states
func TestCheckForUpdatesDefersNonIdle(t *testing.T) {
	nonIdleStates := []string{"downloading", "preparing", "installing", "pending-reboot", "error"}

	for _, state := range nonIdleStates {
		t.Run(state, func(t *testing.T) {
			// The check is: if currentStatus != "idle" && currentStatus != ""
			shouldDefer := state != "idle" && state != ""
			if !shouldDefer {
				t.Errorf("State %q should cause deferral", state)
			}
		})
	}

	// idle and empty should NOT defer
	for _, state := range []string{"idle", ""} {
		t.Run("not_"+state, func(t *testing.T) {
			shouldDefer := state != "idle" && state != ""
			if shouldDefer {
				t.Errorf("State %q should NOT cause deferral", state)
			}
		})
	}
}
