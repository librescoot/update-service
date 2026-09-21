package config

import (
	"slices"
	"sync"
	"testing"
	"time"
)

func TestInferChannelFromVersion(t *testing.T) {
	cases := []struct {
		name    string
		version string
		want    string
	}{
		{"testing tag", "testing-20260313T142530", "testing"},
		{"testing tag lowercase t", "testing-20260426t013148", "testing"},
		{"testing with codename suffix", "testing-20260313T142530 (codename)", "testing"},
		{"nightly tag", "nightly-20260313T142530", "nightly"},
		{"nightly tag lowercase t", "nightly-20260426t013148", "nightly"},
		{"stable v-prefixed", "v1.2.3", "stable"},
		{"stable digit-prefixed", "1.2.3", "stable"},
		{"stable v-prefixed with codename", "v1.2.3 (codename)", "stable"},
		{"custom-nightly is not nightly", "custom-nightly-20260313T142530-some-branch", ""},
		{"empty string", "", ""},
		{"bare codename", "(none)", ""},
		{"unknown prefix", "preview-20260313T142530", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := InferChannelFromVersion(tc.version)
			if got != tc.want {
				t.Errorf("InferChannelFromVersion(%q) = %q, want %q", tc.version, got, tc.want)
			}
		})
	}
}

func TestIsValidChannel(t *testing.T) {
	valid := []string{"stable", "testing", "nightly"}
	for _, ch := range valid {
		if !IsValidChannel(ch) {
			t.Errorf("IsValidChannel(%q) = false, want true", ch)
		}
	}

	invalid := []string{"", "STABLE", "Nightly", "custom-nightly", "preview", "foo"}
	for _, ch := range invalid {
		if IsValidChannel(ch) {
			t.Errorf("IsValidChannel(%q) = true, want false", ch)
		}
	}
}

func TestConfig_BudgetDefaults(t *testing.T) {
	c := New("localhost:6379", "https://example.invalid", time.Hour, "mdb", "stable", "/data/ota/mdb", false, false, "/uboot", "", 2)
	if c.DownloadMaxDuration != 60*time.Minute {
		t.Errorf("DownloadMaxDuration = %v, want 60m", c.DownloadMaxDuration)
	}
	if c.DownloadStallWindow != 2*time.Minute {
		t.Errorf("DownloadStallWindow = %v, want 2m", c.DownloadStallWindow)
	}
	if c.DownloadStallMinBytes != 65536 {
		t.Errorf("DownloadStallMinBytes = %d, want 65536", c.DownloadStallMinBytes)
	}
}

func TestConfig_ApplyRedisUpdate_Budget(t *testing.T) {
	c := New("localhost:6379", "https://example.invalid", time.Hour, "mdb", "stable", "/data/ota/mdb", false, false, "/uboot", "", 2)

	if !c.ApplyRedisUpdate("updates.mdb.download-max-duration", "30m") {
		t.Fatal("download-max-duration should be recognised")
	}
	if c.DownloadMaxDuration != 30*time.Minute {
		t.Errorf("DownloadMaxDuration = %v, want 30m", c.DownloadMaxDuration)
	}

	if !c.ApplyRedisUpdate("updates.mdb.download-stall-min-bytes", "4096") {
		t.Fatal("download-stall-min-bytes should be recognised")
	}
	if c.DownloadStallMinBytes != 4096 {
		t.Errorf("DownloadStallMinBytes = %d, want 4096", c.DownloadStallMinBytes)
	}

	// 0 disables a budget, matching how check-interval treats 0.
	if !c.ApplyRedisUpdate("updates.mdb.download-max-duration", "0") {
		t.Fatal("0 should be accepted")
	}
	if c.DownloadMaxDuration != 0 {
		t.Errorf("DownloadMaxDuration = %v, want 0 (disabled)", c.DownloadMaxDuration)
	}

	// Garbage must be ignored rather than zeroing the budget.
	c.DownloadStallWindow = 2 * time.Minute
	if c.ApplyRedisUpdate("updates.mdb.download-stall-window", "not-a-duration") {
		t.Error("invalid duration should not be applied")
	}
	if c.DownloadStallWindow != 2*time.Minute {
		t.Errorf("DownloadStallWindow = %v, want the previous value retained", c.DownloadStallWindow)
	}

	// Another component's setting must not leak across.
	if c.ApplyRedisUpdate("updates.dbc.download-max-duration", "5m") {
		t.Error("a dbc setting must not apply to the mdb config")
	}
}

func TestConfig_CommitGateDefaults(t *testing.T) {
	mdb := New("localhost:6379", "https://example.invalid", time.Hour, "mdb", "stable", "/data/ota/mdb", false, false, "/uboot", "", 2)
	mdbGate := mdb.CommitGateSettings()
	if mdbGate.Enabled {
		t.Error("the gate must be off unless a device opts in")
	}
	if mdbGate.Floor != DefaultCommitGateFloor {
		t.Errorf("Floor = %v, want %v", mdbGate.Floor, DefaultCommitGateFloor)
	}
	if mdbGate.Deadline != DefaultCommitGateDeadline {
		t.Errorf("Deadline = %v, want %v", mdbGate.Deadline, DefaultCommitGateDeadline)
	}

	// The MDB is the only component with pm-service in its required set; a
	// wrong entry here is what turns a good update into a rollback.
	want := []string{"valkey.service", "librescoot-vehicle.service", "librescoot-settings.service", "librescoot-version.service", "librescoot-pm.service"}
	if !slices.Equal(mdbGate.RequiredUnits, want) {
		t.Errorf("mdb required units = %v, want %v", mdbGate.RequiredUnits, want)
	}

	dbc := New("localhost:6379", "https://example.invalid", time.Hour, "dbc", "stable", "/data/ota/dbc", false, false, "/uboot", "", 2)
	dbcGate := dbc.CommitGateSettings()
	if slices.Contains(dbcGate.RequiredUnits, "librescoot-pm.service") {
		t.Errorf("dbc required units must not include pm-service: %v", dbcGate.RequiredUnits)
	}
}

func TestConfig_ApplyRedisUpdate_CommitGate(t *testing.T) {
	c := New("localhost:6379", "https://example.invalid", time.Hour, "mdb", "stable", "/data/ota/mdb", false, false, "/uboot", "", 2)

	if !c.ApplyRedisUpdate("updates.mdb.commit-gate", "true") {
		t.Fatal("commit-gate should be recognised")
	}
	if gate := c.CommitGateSettings(); !gate.Enabled {
		t.Error("enabling the gate setting did not take effect")
	}

	if !c.ApplyRedisUpdate("updates.mdb.commit-gate-floor", "90s") {
		t.Fatal("commit-gate-floor should be recognised")
	}
	if got := c.CommitGateSettings().Floor; got != 90*time.Second {
		t.Errorf("Floor = %v, want 90s", got)
	}

	if !c.ApplyRedisUpdate("updates.mdb.commit-gate-deadline", "45m") {
		t.Fatal("commit-gate-deadline should be recognised")
	}
	if got := c.CommitGateSettings().Deadline; got != 45*time.Minute {
		t.Errorf("Deadline = %v, want 45m", got)
	}

	// A deadline of zero would expire before the first probe tick, which is a
	// fail-closed rollback on every update. Reject it and keep the old value.
	if c.ApplyRedisUpdate("updates.mdb.commit-gate-deadline", "0") {
		t.Error("a zero deadline must not be applied")
	}
	if got := c.CommitGateSettings().Deadline; got != 45*time.Minute {
		t.Errorf("Deadline = %v, want the previous value retained", got)
	}

	// A negative floor would let the probes run before boot has settled.
	if c.ApplyRedisUpdate("updates.mdb.commit-gate-floor", "-1m") {
		t.Error("a negative floor must not be applied")
	}

	if !c.ApplyRedisUpdate("updates.mdb.commit-gate-required-units", "valkey.service, librescoot-vehicle.service") {
		t.Fatal("commit-gate-required-units should be recognised")
	}
	if got := c.CommitGateSettings().RequiredUnits; !slices.Equal(got, []string{"valkey.service", "librescoot-vehicle.service"}) {
		t.Errorf("RequiredUnits = %v, want the two listed units", got)
	}

	// Clearing the setting restores the component default rather than
	// requiring every unit, which would block commits forever.
	if !c.ApplyRedisUpdate("updates.mdb.commit-gate-required-units", "") {
		t.Fatal("an empty required-units value should reset to the default")
	}
	if got := c.CommitGateSettings().RequiredUnits; !slices.Contains(got, "librescoot-pm.service") {
		t.Errorf("RequiredUnits = %v, want the mdb default set", got)
	}

	// Garbage must be ignored rather than disabling the gate or zeroing the window.
	if c.ApplyRedisUpdate("updates.mdb.commit-gate", "maybe") {
		t.Error("an unparsable bool must not be applied")
	}
	if !c.CommitGateSettings().Enabled {
		t.Error("the previous enabled value should be retained")
	}

	// Another component's setting must not leak across.
	if c.ApplyRedisUpdate("updates.dbc.commit-gate", "true") {
		t.Error("a dbc setting must not apply to the mdb config")
	}
}

// TestCommitGateSettings_ConcurrentWithApplyRedisUpdate mirrors the download
// budget race test: gateMu is what makes the per-tick snapshot safe while the
// settings watcher rewrites the fields.
func TestCommitGateSettings_ConcurrentWithApplyRedisUpdate(t *testing.T) {
	c := New("localhost:6379", "https://example.invalid", time.Hour, "mdb", "stable", "/data/ota/mdb", false, false, "/uboot", "", 2)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = c.CommitGateSettings()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			c.ApplyRedisUpdate("updates.mdb.commit-gate-floor", "10s")
			c.ApplyRedisUpdate("updates.mdb.commit-gate-required-units", "valkey.service")
		}
	}()
	wg.Wait()
}

func TestParseUnitList(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  []string
	}{
		{"commas", "a.service,b.service", []string{"a.service", "b.service"}},
		{"spaces", "a.service b.service", []string{"a.service", "b.service"}},
		{"mixed with padding", " a.service,b.service , c.service ", []string{"a.service", "b.service", "c.service"}},
		{"empty", "", []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseUnitList(tc.value)
			if len(got) != len(tc.want) {
				t.Fatalf("ParseUnitList(%q) = %v, want %v", tc.value, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("ParseUnitList(%q)[%d] = %q, want %q", tc.value, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestDownloadBudget_ConcurrentWithApplyRedisUpdate exercises exactly the
// pattern a download attempt sees in production: one goroutine reading the
// budget through DownloadBudget() in a loop (standing in for the download
// goroutine snapshotting it at the top of each attempt) while another
// rewrites it via ApplyRedisUpdate (standing in for the settings-watcher
// goroutine). Run with -race: budgetMu is what makes this safe.
func TestDownloadBudget_ConcurrentWithApplyRedisUpdate(t *testing.T) {
	c := New("localhost:6379", "https://example.invalid", time.Hour, "mdb", "stable", "/data/ota/mdb", false, false, "/uboot", "", 2)

	const iterations = 2000
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			d := time.Duration(i%5+1) * time.Minute
			if !c.ApplyRedisUpdate("updates.mdb.download-max-duration", d.String()) {
				t.Error("download-max-duration should be recognised")
			}
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			maxDuration, _, _ := c.DownloadBudget()
			if maxDuration <= 0 {
				t.Error("DownloadBudget should never observe a torn/zero read from these writes")
			}
		}
	}()

	wg.Wait()
}
