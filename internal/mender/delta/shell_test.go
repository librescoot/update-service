package delta

import (
	"errors"
	"os/exec"
	"reflect"
	"testing"
)

// stubLookPath makes only the named binaries appear installed.
func stubLookPath(t *testing.T, present ...string) {
	t.Helper()
	set := make(map[string]bool, len(present))
	for _, p := range present {
		set[p] = true
	}
	orig := lookPath
	lookPath = func(name string) (string, error) {
		if set[name] {
			return "/usr/bin/" + name, nil
		}
		return "", errors.New("not found")
	}
	t.Cleanup(func() { lookPath = orig })
}

// The priority wrappers are a courtesy, not a requirement: a rootfs without
// util-linux must still be able to apply a delta update.
func TestLowPriorityArgsFallsBack(t *testing.T) {
	tests := []struct {
		name     string
		present  []string
		wantBin  string
		wantArgs []string
	}{
		{
			name:     "ionice and nice available",
			present:  []string{"ionice", "nice"},
			wantBin:  "ionice",
			wantArgs: []string{"-c3", "nice", "-n", "19", "gzip", "-9"},
		},
		{
			name:     "only nice available",
			present:  []string{"nice"},
			wantBin:  "nice",
			wantArgs: []string{"-n", "19", "gzip", "-9"},
		},
		{
			name:     "neither available: run it anyway",
			present:  nil,
			wantBin:  "gzip",
			wantArgs: []string{"-9"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubLookPath(t, tt.present...)
			bin, args := lowPriorityArgs("gzip", "-9")
			if bin != tt.wantBin {
				t.Errorf("bin = %q, want %q", bin, tt.wantBin)
			}
			if !reflect.DeepEqual(args, tt.wantArgs) {
				t.Errorf("args = %v, want %v", args, tt.wantArgs)
			}
		})
	}
}

// The command actually built must be runnable, not just well-formed.
func TestLowPriorityCommandRunsWithoutWrappers(t *testing.T) {
	stubLookPath(t)
	if _, err := exec.LookPath("echo"); err != nil {
		t.Skip("echo not available")
	}
	if err := lowPriorityCommand("echo", "ok").Run(); err != nil {
		t.Errorf("command failed with no priority wrappers present: %v", err)
	}
}
