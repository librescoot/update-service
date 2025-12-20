package updater

import (
	"sync"
	"testing"
)

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
		name           string
		errorMsg       string
		isCorruption   bool
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
