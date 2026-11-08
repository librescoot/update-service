package updater

import (
	"strconv"
	"strings"
	"testing"
)

// A file install dropped because the updater is busy must be reported to the
// BLE receiver through install-refused:<component>, without touching the
// status and error fields the commit gate reads as evidence about an image.
func TestHandleUpdateFromFileBusyRecordsRefusal(t *testing.T) {
	u, mr, installs := newStagedTestUpdater(t, "mdb")
	dir := u.mender.GetDownloadDir()
	mr.HSet("version:mdb", "version_id", "nightly-20260101t000000")
	src := writeBaseMender(t, dir, "librescoot-unu-mdb-nightly-20260102T000000.mender", strings.Repeat("a", 64))

	mr.HSet("ota", "status:mdb", "idle")
	u.updateOpMu.Lock()
	u.handleUpdateFromFile(src)
	u.updateOpMu.Unlock()

	value := mr.HGet("ota", "install-refused:mdb")
	sep := strings.IndexByte(value, ':')
	if sep <= 0 {
		t.Fatalf("install-refused:mdb = %q", value)
	}
	if _, err := strconv.ParseInt(value[:sep], 10, 64); err != nil {
		t.Fatalf("install-refused timestamp %q: %v", value[:sep], err)
	}
	if reason := value[sep+1:]; reason == "" {
		t.Fatal("install-refused carries no reason")
	}
	if got := mr.HGet("ota", "status:mdb"); got != "idle" {
		t.Errorf("status:mdb = %q, want untouched idle", got)
	}
	if mr.HGet("ota", "error:mdb") != "" {
		t.Error("refusal wrote the error field the commit gate reads")
	}
	if len(*installs) != 0 {
		t.Fatalf("install ran while the updater was busy: %v", *installs)
	}
}

// The same refusal while the commit gate is evaluating a pending update.
func TestHandleUpdateFromFileGateWindowRecordsRefusal(t *testing.T) {
	u, mr, installs := newStagedTestUpdater(t, "mdb")
	dir := u.mender.GetDownloadDir()
	mr.HSet("version:mdb", "version_id", "nightly-20260101t000000")
	src := writeBaseMender(t, dir, "librescoot-unu-mdb-nightly-20260102T000000.mender", strings.Repeat("a", 64))

	u.gateWindowMu.Lock()
	u.gateWindowOpen = true
	u.gateWindowMu.Unlock()

	u.handleUpdateFromFile(src)

	if value := mr.HGet("ota", "install-refused:mdb"); !strings.Contains(value, ":") {
		t.Fatalf("install-refused:mdb = %q", value)
	}
	if got := mr.HGet("ota", "status:mdb"); got != "" {
		t.Errorf("status:mdb = %q, want untouched", got)
	}
	if len(*installs) != 0 {
		t.Fatalf("install ran during a gate window: %v", *installs)
	}
}
