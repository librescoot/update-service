package updater

import "testing"

func TestCheckForUpdatesWithoutChannelReportsManualError(t *testing.T) {
	u, mr := newTestUpdaterForPreview(t, nil)
	u.config.Channel = ""

	u.checkForUpdates(true)

	waitForField(t, mr, "ota", "status:mdb", "error")
	if got := mr.HGet("ota", "error:mdb"); got != "channel-not-configured" {
		t.Fatalf("error:mdb = %q, want channel-not-configured", got)
	}
}

func TestManualCheckWithoutChannelDoesNotOverwriteActiveOperation(t *testing.T) {
	u, mr := newTestUpdaterForPreview(t, nil)
	u.config.Channel = ""
	u.updateOpMu.Lock()
	defer u.updateOpMu.Unlock()

	u.checkForUpdates(true)

	if got := mr.HGet("ota", "status:mdb"); got != "" {
		t.Fatalf("status:mdb = %q, want unchanged", got)
	}
}

func TestPeriodicCheckWithoutChannelDoesNotPublishError(t *testing.T) {
	u, mr := newTestUpdaterForPreview(t, nil)
	u.config.Channel = ""

	u.checkForUpdates(false)

	if got := mr.HGet("ota", "status:mdb"); got != "" {
		t.Fatalf("status:mdb = %q, want unchanged", got)
	}
}
