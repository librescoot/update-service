package updater

import (
	"fmt"
	"testing"

	"github.com/alicebob/miniredis/v2"
)

func setting(mr *miniredis.Miniredis, component, field string) string {
	return mr.HGet("settings", fmt.Sprintf("updates.%s.%s", component, field))
}

// TestCheckAttemptDoesNotCompleteOnUnconfiguredChannel covers the distinction
// the attempt fields exist for: a check that could not run must record the
// attempt and its result without moving last-check-time.
func TestCheckAttemptDoesNotCompleteOnUnconfiguredChannel(t *testing.T) {
	u, mr := newTestUpdaterForPreview(t, stableIndex())
	// An invalid configured channel fails before any release lookup.
	mr.HSet("settings", "updates.mdb.channel", "bogus")

	u.checkForUpdates(false)

	if got := setting(mr, "mdb", "last-check-time"); got != "" {
		t.Errorf("last-check-time = %q, want empty (check did not run)", got)
	}
	if got := setting(mr, "mdb", "last-attempt-time"); got == "" {
		t.Error("last-attempt-time not recorded")
	}
	if got := setting(mr, "mdb", "last-attempt-result"); got != checkResultNoChannel {
		t.Errorf("last-attempt-result = %q, want %q", got, checkResultNoChannel)
	}
}

// TestCheckCompletesWithNoRelease covers a check that ran to a decision but
// found nothing for this variant.
func TestCheckCompletesWithNoRelease(t *testing.T) {
	u, mr := newTestUpdaterForPreview(t, stableIndex())
	mr.HSet("settings", "updates.mdb.channel", "stable")
	mr.HSet("settings", "updates.mdb.method", "full")

	u.checkForUpdates(false)

	if got := setting(mr, "mdb", "last-attempt-result"); got != checkResultNoRelease {
		t.Errorf("last-attempt-result = %q, want %q", got, checkResultNoRelease)
	}
	if got := setting(mr, "mdb", "last-check-time"); got == "" {
		t.Error("last-check-time not recorded for a completed check")
	}
}

// TestCheckCompletesUpToDate covers the normal no-op check result.
func TestCheckCompletesUpToDate(t *testing.T) {
	u, mr := newTestUpdaterForPreview(t, stableIndex())
	mr.HSet("settings", "updates.mdb.channel", "stable")
	mr.HSet("settings", "updates.mdb.method", "full")
	mr.HSet("version:mdb", "version_id", "v1.3.0")
	mr.HSet("version:mdb", "variant_id", "unu-mdb")

	u.checkForUpdates(false)

	if got := setting(mr, "mdb", "last-attempt-result"); got != checkResultUpToDate {
		t.Errorf("last-attempt-result = %q, want %q", got, checkResultUpToDate)
	}
	if got := setting(mr, "mdb", "last-check-time"); got == "" {
		t.Error("last-check-time not recorded for a completed check")
	}
}
