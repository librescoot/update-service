package updater

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/librescoot/update-service/internal/catalog"
	"github.com/librescoot/update-service/internal/config"
	"github.com/librescoot/update-service/internal/mender"
)

func TestExclusiveFeedNeverFetchesLegacyOnFailure(t *testing.T) {
	u, mr := newTestUpdaterForPreview(t, nil)
	dir := t.TempDir()
	u.mender = mender.NewManager(dir, nil, u.logger)
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		http.Error(w, "not published", http.StatusNotFound)
	}))
	defer srv.Close()
	p := config.FeedPolicy{Schema: 2, URL: srv.URL + "/updates/v2/canary.json", Channel: "testing"}
	file := filepath.Join(dir, "feed.json")
	if err := config.SaveFeedPolicy(file, p); err != nil {
		t.Fatal(err)
	}
	if err := u.config.BindFeed(file, p); err != nil {
		t.Fatal(err)
	}
	u.githubAPI = NewGitHubAPI(context.Background(), srv.URL+"/legacy", u.logger)
	u.githubAPI.client = srv.Client()
	mr.HSet("settings", "updates.mdb.channel", "stable")
	mr.HSet("settings", "updates.mdb.releases-url", srv.URL+"/legacy")
	mr.HSet("version:mdb", "version_id", "v1.4.1")
	mr.HSet("version:mdb", "variant_id", "unu-mdb")
	u.checkForUpdates(true)
	u.wg.Wait()
	u.previewChannel("testing")
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 2 {
		t.Fatalf("requests: %v", paths)
	}
	for _, path := range paths {
		if path != "/updates/v2/canary.json" {
			t.Fatalf("legacy fallback: %v", paths)
		}
	}
	if got := mr.HGet("settings", "updates.mdb.last-attempt-result"); got != checkResultReleasesFailed {
		t.Fatalf("wrong failure %s", got)
	}
	if ch, err := u.resolveChannel(); err != nil || ch != "testing" {
		t.Fatalf("transient channel escaped enrollment: %s %v", ch, err)
	}
}
func TestEnrollmentPreventsInstallingOldUpdater(t *testing.T) {
	u, _ := newTestUpdaterForPreview(t, nil)
	dir := t.TempDir()
	u.mender = mender.NewManager(dir, nil, u.logger)
	policy := config.FeedPolicy{Schema: 2, URL: config.CanaryV2Feed, Channel: "testing"}
	file := filepath.Join(dir, "feed.json")
	if err := config.SaveFeedPolicy(file, policy); err != nil {
		t.Fatal(err)
	}
	if err := u.config.BindFeed(file, policy); err != nil {
		t.Fatal(err)
	}
	image, _, _, _ := fixtureImage(t, dir, "v1.4.1", []byte("old updater image"))
	called := false
	u.installArtifact = func(string, mender.InstallProgressCallback) error { called = true; return nil }
	if err := u.installMender(image); err == nil {
		t.Fatal("unsupported target was accepted")
	}
	if called {
		t.Fatal("installer ran before checking target capability")
	}
}

func TestExclusiveTargetRequiresCapableUpdater(t *testing.T) {
	u, _ := newTestUpdaterForPreview(t, nil)
	a := planArtifact("v1.4.1")
	c := catalog.Catalog{Artifacts: []catalog.Artifact{a}, Targets: map[string]map[string]string{"stable": {"unu-mdb": a.ID}}}
	if _, err := u.exclusiveTarget(&c, "stable", "unu-mdb"); err == nil {
		t.Fatal("old updater target accepted")
	}
	c.Artifacts[0].UpdaterProtocol = config.OTAProtocol
	if _, err := u.exclusiveTarget(&c, "stable", "unu-mdb"); err != nil {
		t.Fatal(err)
	}
}
func TestJointFeedSwitchRequiresMatchingEnrollment(t *testing.T) {
	u, mr := newTestUpdaterForPreview(t, nil)
	dir := t.TempDir()
	u.mender = mender.NewManager(dir, nil, u.logger)
	p := config.FeedPolicy{Schema: 2, URL: config.CanaryV2Feed, Channel: "testing"}
	file := filepath.Join(dir, "feed.json")
	if err := config.SaveFeedPolicy(file, p); err != nil {
		t.Fatal(err)
	}
	if err := u.config.BindFeed(file, p); err != nil {
		t.Fatal(err)
	}
	u.observeUpdate = func() (mender.UpdateObservation, error) {
		return mender.UpdateObservation{State: mender.StateNoUpdate}, nil
	}
	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	mr.HSet("ota", "feed-url:mdb", config.CanaryV2Feed)
	mr.HSet("ota", "feed-url:dbc", config.DefaultV2Feed)
	u.requestSwitch("stable", id)
	if mr.HGet("ota", "switch-status") != "error" {
		t.Fatal("mixed enrollments accepted")
	}
	mr.HSet("ota", "feed-url:dbc", config.CanaryV2Feed)
	u.requestSwitch("stable", id)
	if mr.HGet("ota", "switch-status") != "accepted" {
		t.Fatal("matching enrollments refused")
	}
	u.advanceSwitch()
	saved, err := config.LoadFeedPolicy(file)
	if err != nil || saved.Channel != "stable" || saved.URL != config.CanaryV2Feed {
		t.Fatalf("lost feed on channel switch: %+v %v", saved, err)
	}
}

func TestFeedSwitchOnlyChangesPersistentChannel(t *testing.T) {
	u, mr := newTestUpdaterForPreview(t, nil)
	dir := t.TempDir()
	u.mender = mender.NewManager(dir, nil, u.logger)
	p := config.FeedPolicy{Schema: 2, URL: config.CanaryV2Feed, Channel: "testing"}
	file := filepath.Join(dir, "feed.json")
	if err := config.SaveFeedPolicy(file, p); err != nil {
		t.Fatal(err)
	}
	if err := u.config.BindFeed(file, p); err != nil {
		t.Fatal(err)
	}
	u.observeUpdate = func() (mender.UpdateObservation, error) {
		return mender.UpdateObservation{State: mender.StateNoUpdate}, nil
	}
	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	u.setPersistentFeedChannel("stable", id)
	saved, err := config.LoadFeedPolicy(file)
	if err != nil || saved.Channel != "stable" || saved.URL != config.CanaryV2Feed {
		t.Fatalf("wrong persistent switch: %+v %v", saved, err)
	}
	if mr.HGet("ota", "feed-channel-result:mdb") != id+":accepted" {
		t.Fatal("missing durable acceptance")
	}
	u.setPersistentFeedChannel("nightly", id)
	saved, _ = config.LoadFeedPolicy(file)
	if saved.Channel != "stable" {
		t.Fatal("unsupported channel persisted")
	}
}
