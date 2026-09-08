package updater

import (
	"bytes"
	"strings"
	"testing"

	"github.com/librescoot/update-service/internal/backoff"
	"github.com/librescoot/update-service/internal/status"
)

func TestChannelSwitchEligibility(t *testing.T) {
	versions := map[string]string{
		"stable":  "v1.3.0",
		"testing": "testing-20260820T120000",
		"nightly": "nightly-20260820T120000",
	}
	for _, component := range []string{"mdb", "dbc"} {
		for from, installed := range versions {
			for to, target := range versions {
				t.Run(component+"/"+from+"-to-"+to, func(t *testing.T) {
					u, mr := newTestUpdaterForPreview(t, nil)
					u.config.Component = component
					// A delayed watcher still knows the old channel. Eligibility must be
					// based on the selected release, not this mutable cache.
					u.config.Channel = from
					mr.HSet("version:"+component, "version_id", installed)
					want := from != to
					if got := u.isUpdateNeeded(Release{TagName: target}); got != want {
						t.Fatalf("isUpdateNeeded(%s -> %s) = %v, want %v", installed, target, got, want)
					}
					if got := u.isVersionNewer(target, installed, to); got != want {
						t.Fatalf("DBC eligibility(%s -> %s) = %v, want %v", installed, target, got, want)
					}
				})
			}
		}
	}
}

func TestSameChannelVersionPolicyPreserved(t *testing.T) {
	for _, tc := range []struct {
		current, target string
		want            bool
	}{
		{"v1.2.9", "v1.3.0", true},
		{"1.3.0", "v1.3.0", false},
		{"v1.4.0", "v1.3.0", false},
		{"20260820T120000", "nightly-20260820T120000", false},
		{"nightly-20260819T120000", "nightly-20260820T120000", true},
	} {
		t.Run(tc.current+"/"+tc.target, func(t *testing.T) {
			u, mr := newTestUpdaterForPreview(t, nil)
			mr.HSet("version:mdb", "version_id", tc.current)
			if got := u.isUpdateNeeded(Release{TagName: tc.target}); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCheckSettingsReadRedisBeforeWatcher(t *testing.T) {
	for _, component := range []string{"mdb", "dbc"} {
		t.Run(component, func(t *testing.T) {
			u, mr := newTestUpdaterForPreview(t, nil)
			u.config.Component = component
			u.config.Channel = "nightly"
			u.updateMethod = "delta"
			mr.HSet("settings", "updates."+component+".channel", "stable")
			mr.HSet("settings", "updates."+component+".method", "full")
			channel, method, err := u.resolveCheckSettings()
			if err != nil || channel != "stable" || method != "full" {
				t.Fatalf("settings = %q, %q, %v", channel, method, err)
			}
			// Later notifications cannot mutate the operation's snapshot.
			u.config.ApplyRedisUpdate("updates."+component+".channel", "testing")
			if channel != "stable" {
				t.Fatal("selected channel changed")
			}
			u.config.ChannelFromCLI = true
			channel, _, err = u.resolveCheckSettings()
			if err != nil || channel != "testing" {
				t.Fatalf("CLI channel overridden: %q, %v", channel, err)
			}
		})
	}
}

func TestCheckNowSwitchesToStableWithoutWatcher(t *testing.T) {
	for _, component := range []string{"mdb", "dbc"} {
		t.Run(component, func(t *testing.T) {
			u, mr := newTestUpdaterForPreview(t, stableIndex())
			u.config.Component = component
			u.status = status.NewReporter(u.redis.GetClient(), component, u.logger)
			u.config.Channel = "nightly"
			u.backoff = backoff.NewStore(t.TempDir(), u.logger)
			mr.HSet("settings", "updates."+component+".channel", "stable")
			mr.HSet("version:"+component, "version_id", "nightly-20260820T120000")
			mr.HSet("version:"+component, "variant_id", "unu-"+component)
			var output bytes.Buffer
			u.logger.SetOutput(&output)
			// Exercise release selection and scheduling, but never install anything.
			u.updateOpMu.Lock()
			u.checkForUpdates(true)
			u.wg.Wait()
			u.updateOpMu.Unlock()
			logs := output.String()
			for _, want := range []string{"on channel stable", "Forcing full update", "(using full update)"} {
				if !strings.Contains(logs, want) {
					t.Fatalf("missing %q in:\n%s", want, logs)
				}
			}
		})
	}
}

func TestRemovedChannelOverrideUsesStartupFallback(t *testing.T) {
	for _, fallback := range []string{"stable", ""} {
		for _, deleted := range []bool{true, false} {
			u, mr := newTestUpdaterForPreview(t, nil)
			u.config.FallbackChannel = fallback
			u.config.Channel = "testing" // previous startup/watcher override
			mr.HSet("settings", "updates.mdb.channel", "testing")
			if deleted {
				mr.HDel("settings", "updates.mdb.channel")
			} else {
				mr.HSet("settings", "updates.mdb.channel", "")
			}
			channel, _, err := u.resolveCheckSettings()
			if err != nil || channel != fallback {
				t.Fatalf("deleted=%v, fallback=%q: got %q, %v", deleted, fallback, channel, err)
			}
		}
	}
}

func TestCheckSettingsFailClosed(t *testing.T) {
	u, mr := newTestUpdaterForPreview(t, nil)
	mr.Set("settings", "not-a-hash")
	if _, _, err := u.resolveCheckSettings(); err == nil {
		t.Fatal("Redis read error silently fell back to old channel")
	}
	u.config.ChannelFromCLI = true
	if _, _, err := u.resolveCheckSettings(); err == nil {
		t.Fatal("Redis method read error silently fell back to delta")
	}
}
