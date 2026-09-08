package mender

import (
	"strings"
	"testing"

	menderstatus "github.com/librescoot/librescoot-mender-status/mender"
)

func TestUpdateState_Constants(t *testing.T) {
	// Ensure the constants have distinct values
	states := []UpdateState{StateNoUpdate, StateCommitted, StateNeedsReboot, StateNeedsCommit, StateNeedsResume, StateInconsistent}
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

func TestVersionFromArtifact(t *testing.T) {
	tests := map[string]string{
		"release-v1.4.0":                         "v1.4.0",
		"release-nightly-20260908t063233":        "nightly-20260908t063233",
		"release-v1.4.0-minimal":                 "v1.4.0",
		"release-v1.4.0_INCONSISTENT":            "v1.4.0",
		"release-v1.4.0-minimal_INCONSISTENT":    "v1.4.0",
		"custom-artifact-without-release-prefix": "custom-artifact-without-release-prefix",
	}
	for artifact, want := range tests {
		if got := VersionFromArtifact(artifact); got != want {
			t.Errorf("VersionFromArtifact(%q) = %q, want %q", artifact, got, want)
		}
	}
}

func TestObserveStatus(t *testing.T) {
	needsCommit := &menderstatus.Status{
		CommittedArtifact: "release-v1.3.1",
		UpdateInProgress:  true,
		State: &menderstatus.StandaloneState{
			ArtifactName: "release-v1.4.0",
			InState:      menderstatus.StateBeforeArtifactCommitEnter,
		},
	}
	got := observeStatus(needsCommit)
	if got.State != StateNeedsCommit || got.CommittedVersion != "v1.3.1" ||
		got.PendingVersion != "v1.4.0" || got.PendingArtifact != "release-v1.4.0" {
		t.Fatalf("unexpected observation: %+v", got)
	}

	needsCommit.State.InState = menderstatus.StateArtifactCommitLeave
	if got := observeStatus(needsCommit); got.State != StateNeedsResume {
		t.Fatalf("cleanup state = %v, want StateNeedsResume", got.State)
	}

	needsCommit.State.Failed = true
	if got := observeStatus(needsCommit); got.State != StateNeedsResume {
		t.Fatalf("failed standalone state = %v, want StateNeedsResume", got.State)
	}
}

func TestInstallerObserveUpdateUsesInjectedReader(t *testing.T) {
	installer := &Installer{readStatus: func() (*menderstatus.Status, error) {
		return &menderstatus.Status{CommittedArtifact: "release-v1.4.0"}, nil
	}}
	got, err := installer.ObserveUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateNoUpdate || got.CommittedVersion != "v1.4.0" {
		t.Fatalf("unexpected observation: %+v", got)
	}
}

func TestInconsistentSuffix(t *testing.T) {
	testCases := []struct {
		artifact       string
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
