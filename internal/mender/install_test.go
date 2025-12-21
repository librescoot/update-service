package mender

import (
	"strings"
	"testing"
)

func TestUpdateState_Constants(t *testing.T) {
	// Ensure the constants have distinct values
	states := []UpdateState{StateNoUpdate, StateCommitted, StateNeedsReboot, StateInconsistent}
	seen := make(map[UpdateState]bool)

	for _, state := range states {
		if seen[state] {
			t.Errorf("Duplicate UpdateState value: %d", state)
		}
		seen[state] = true
	}
}

func TestCommitResult_ExitCodes(t *testing.T) {
	testCases := []struct {
		name       string
		result     CommitResult
		wantReboot bool
		wantNoOp   bool
	}{
		{
			name:       "success",
			result:     CommitResult{Success: true, ExitCode: 0},
			wantReboot: false,
			wantNoOp:   false,
		},
		{
			name:       "no update in progress",
			result:     CommitResult{Success: false, ExitCode: 2},
			wantReboot: false,
			wantNoOp:   true,
		},
		{
			name:       "needs reboot",
			result:     CommitResult{Success: false, ExitCode: 1},
			wantReboot: true,
			wantNoOp:   false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Test the exit code interpretation logic
			needsReboot := !tc.result.Success && tc.result.ExitCode == 1
			noOp := !tc.result.Success && tc.result.ExitCode == 2

			if needsReboot != tc.wantReboot {
				t.Errorf("needsReboot = %v, want %v", needsReboot, tc.wantReboot)
			}
			if noOp != tc.wantNoOp {
				t.Errorf("noOp = %v, want %v", noOp, tc.wantNoOp)
			}
		})
	}
}

func TestInconsistentSuffix(t *testing.T) {
	testCases := []struct {
		artifact      string
		isInconsistent bool
	}{
		{"release-nightly-20251211T024757", false},
		{"release-nightly-20251211T024757_INCONSISTENT", true},
		{"unknown", false},
		{"unknown_INCONSISTENT", true},
	}

	for _, tc := range testCases {
		t.Run(tc.artifact, func(t *testing.T) {
			hasInconsistentSuffix := strings.HasSuffix(tc.artifact, "_INCONSISTENT")
			if hasInconsistentSuffix != tc.isInconsistent {
				t.Errorf("artifact %q: hasInconsistentSuffix = %v, want %v",
					tc.artifact, hasInconsistentSuffix, tc.isInconsistent)
			}
		})
	}
}
