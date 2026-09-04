package updater

import "testing"

// A DBC that finished installing is powered off in stand-by before it can
// reboot, so pending-reboot is the normal end of an update. It must survive the
// power-off (the DBC's own recoverFromStuckState acts on it at next boot) and
// must not stop orchestration from powering the dashboard back on, which is
// what activates the staged image.
func TestPendingRebootSurvivesDashboardPowerOff(t *testing.T) {
	if dbcStateIsStaleOnPowerOff("pending-reboot") {
		t.Error("pending-reboot cleared on power-off: the staged image loses its status and the DBC boots into idle, skipping recoverFromStuckState")
	}
	if dbcStatusBlocksOrchestration("pending-reboot") {
		t.Error("pending-reboot treated as busy: nothing powers the DBC back on, so the staged image is stranded")
	}
}

func TestDBCStateIsStaleOnPowerOff(t *testing.T) {
	tests := []struct {
		status string
		want   bool
	}{
		// Resume data lives on disk, so clearing these loses nothing.
		{"downloading", true},
		{"preparing", true},

		// Only the DBC can decide these.
		{"installing", false},
		{"pending-reboot", false},

		{"idle", false},
		{"error", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := dbcStateIsStaleOnPowerOff(tt.status); got != tt.want {
			t.Errorf("dbcStateIsStaleOnPowerOff(%q) = %v, want %v", tt.status, got, tt.want)
		}
	}
}

func TestDBCStatusBlocksOrchestration(t *testing.T) {
	tests := []struct {
		status string
		want   bool
	}{
		{"downloading", true},
		{"preparing", true},
		{"installing", true},
		{"error", true},

		{"pending-reboot", false},
		{"idle", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := dbcStatusBlocksOrchestration(tt.status); got != tt.want {
			t.Errorf("dbcStatusBlocksOrchestration(%q) = %v, want %v", tt.status, got, tt.want)
		}
	}
}
