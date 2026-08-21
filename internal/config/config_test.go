package config

import (
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
