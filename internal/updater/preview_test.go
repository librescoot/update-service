package updater

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	ipc "github.com/librescoot/redis-ipc"

	"github.com/librescoot/update-service/internal/config"
	"github.com/librescoot/update-service/internal/redis"
	"github.com/librescoot/update-service/internal/status"
	"github.com/librescoot/update-service/internal/version"
)

// newTestUpdaterForPreview builds an Updater with a real GitHubAPI pointed at
// a local release index and a real status.Reporter and redis.Client backed by
// miniredis. previewChannel only reads, so this is everything it touches.
func newTestUpdaterForPreview(t *testing.T, index map[string][]Release) (*Updater, *miniredis.Miniredis) {
	t.Helper()
	var manifest map[string]Release
	if index != nil {
		manifest = latestManifest(index)
	}
	return newTestUpdaterForPreviewWithManifest(t, index, manifest)
}

// newTestUpdaterForPreviewWithManifest serves a manifest that need not match the
// per-channel lists, so a test can exercise the two disagreeing.
func newTestUpdaterForPreviewWithManifest(t *testing.T, index map[string][]Release, manifest map[string]Release) (*Updater, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// latest.json carries one release per channel, as the release job
		// publishes it. A nil index stands in for an index that is not there.
		if r.URL.Path == "/latest.json" {
			if manifest == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(manifest)
			return
		}

		// The index is also served as /{channel}.json, matching downloads.librescoot.org.
		// A channel the index does not carry is an empty list rather than a
		// missing document, so the fallback is not an error.
		channel := r.URL.Path
		channel = channel[1:]                         // strip leading /
		channel = channel[:len(channel)-len(".json")] // strip extension
		releases, ok := index[channel]
		if !ok {
			releases = []Release{}
		}
		_ = json.NewEncoder(w).Encode(releases)
	}))
	t.Cleanup(srv.Close)

	client, err := ipc.New(ipc.WithURL(mr.Addr()), ipc.WithCodec(ipc.StringCodec{}))
	if err != nil {
		t.Fatalf("connecting test client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	redisClient, err := redis.New(mr.Addr())
	if err != nil {
		t.Fatalf("connecting test redis client: %v", err)
	}
	t.Cleanup(func() { _ = redisClient.Close() })

	logger := log.New(os.Stdout, "test: ", 0)
	ctx := context.Background()
	u := &Updater{
		config:    &config.Config{Component: "mdb", Channel: "nightly", ReleasesURL: srv.URL},
		redis:     redisClient,
		status:    status.NewReporter(client, "mdb", logger),
		githubAPI: NewGitHubAPI(ctx, srv.URL, logger),
		logger:    logger,
		ctx:       ctx,
	}
	return u, mr
}

// latestManifest derives latest.json from the per-channel index: one release per
// channel, chosen the way the release job chooses it. Stable orders by version,
// the other channels by publish time.
func latestManifest(index map[string][]Release) map[string]Release {
	out := make(map[string]Release, len(index))
	for channel, releases := range index {
		if len(releases) == 0 {
			continue
		}
		newest := releases[0]
		for _, release := range releases[1:] {
			if channel == "stable" {
				if version.Compare(release.TagName, newest.TagName) > 0 {
					newest = release
				}
				continue
			}
			if release.PublishedAt.After(newest.PublishedAt) {
				newest = release
			}
		}
		out[channel] = newest
	}
	return out
}

func stableIndex() map[string][]Release {
	return map[string][]Release{
		"stable": {
			{
				TagName:     "v1.3.0",
				PublishedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
				Assets: []Asset{
					{Name: "librescoot-unu-mdb-v1.3.0.mender", Size: 401234432, URL: "http://example/mdb.mender"},
					{Name: "librescoot-unu-dbc-v1.3.0.mender", Size: 198765432, URL: "http://example/dbc.mender"},
				},
			},
			{
				// Older, and must lose the semver comparison.
				TagName:     "v1.2.9",
				PublishedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
				Assets: []Asset{
					{Name: "librescoot-unu-mdb-v1.2.9.mender", Size: 1, URL: "http://example/old.mender"},
				},
			},
		},
	}
}

func TestPreviewChannel_ReportsLatestReleaseAndSize(t *testing.T) {
	u, mr := newTestUpdaterForPreview(t, stableIndex())
	mr.HSet("version:mdb", "variant_id", "unu-mdb")

	u.previewChannel("stable")

	if got := mr.HGet("ota", "preview-status:mdb"); got != status.PreviewReady {
		t.Errorf("preview-status:mdb = %q, want %q", got, status.PreviewReady)
	}
	if got := mr.HGet("ota", "preview-channel:mdb"); got != "stable" {
		t.Errorf("preview-channel:mdb = %q, want stable", got)
	}
	// v1.3.0 wins on semver despite v1.2.9 having the later publish date:
	// the stable channel orders by version, not by time.
	if got := mr.HGet("ota", "preview-version:mdb"); got != "v1.3.0" {
		t.Errorf("preview-version:mdb = %q, want v1.3.0", got)
	}
	// The MDB's own artifact, not the DBC one that shares the release.
	if got := mr.HGet("ota", "preview-size:mdb"); got != "401234432" {
		t.Errorf("preview-size:mdb = %q, want 401234432", got)
	}
}

// A variant with no artifact in the index is a real case on custom builds:
// the answer is "nothing to switch to", not an error the UI should retry.
func TestPreviewChannel_UnavailableForUnknownVariant(t *testing.T) {
	u, mr := newTestUpdaterForPreview(t, stableIndex())
	mr.HSet("version:mdb", "variant_id", "some-other-board")

	u.previewChannel("stable")

	if got := mr.HGet("ota", "preview-status:mdb"); got != status.PreviewUnavailable {
		t.Errorf("preview-status:mdb = %q, want %q", got, status.PreviewUnavailable)
	}
	if got := mr.HGet("ota", "preview-size:mdb"); got != "" {
		t.Errorf("preview-size:mdb = %q, want empty", got)
	}
}

// A channel whose newest release carries no image for this variant — a board
// that failed to build that night — still offers the release before it.
func TestPreviewChannelFallsBackToTheChannelList(t *testing.T) {
	index := map[string][]Release{
		"stable": {
			{
				TagName:     "v1.4.0",
				PublishedAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
				Assets:      []Asset{{Name: "librescoot-unu-dbc-v1.4.0.mender", Size: 1, URL: "http://example/dbc.mender"}},
			},
			{
				TagName:     "v1.3.5",
				PublishedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
				Assets:      []Asset{{Name: "librescoot-unu-mdb-v1.3.5.mender", Size: 401234432, URL: "http://example/mdb.mender"}},
			},
		},
	}
	manifest := map[string]Release{"stable": index["stable"][0]}
	u, mr := newTestUpdaterForPreviewWithManifest(t, index, manifest)
	mr.HSet("version:mdb", "variant_id", "unu-mdb")

	u.previewChannel("stable")

	if got := mr.HGet("ota", "preview-status:mdb"); got != status.PreviewReady {
		t.Errorf("preview-status:mdb = %q, want %q", got, status.PreviewReady)
	}
	if got := mr.HGet("ota", "preview-version:mdb"); got != "v1.3.5" {
		t.Errorf("preview-version:mdb = %q, want v1.3.5", got)
	}
}

// A manifest that has fallen behind and lost a channel does not take the channel
// down with it: the channel's own list is still consulted.
func TestPreviewChannelFallsBackWhenTheManifestOmitsAChannel(t *testing.T) {
	index := map[string][]Release{
		"stable": {
			{
				TagName:     "v1.3.0",
				PublishedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
				Assets:      []Asset{{Name: "librescoot-unu-mdb-v1.3.0.mender", Size: 401234432, URL: "http://example/mdb.mender"}},
			},
		},
		"testing": {
			{
				TagName:     "testing-20260920T211120",
				PublishedAt: time.Date(2026, 9, 20, 21, 23, 44, 0, time.UTC),
				Prerelease:  true,
				Assets:      []Asset{{Name: "librescoot-unu-mdb-testing-20260920T211120.mender", Size: 228800000, URL: "http://example/mdb.mender"}},
			},
		},
	}
	manifest := map[string]Release{"stable": index["stable"][0]}
	u, mr := newTestUpdaterForPreviewWithManifest(t, index, manifest)
	mr.HSet("version:mdb", "variant_id", "unu-mdb")

	u.previewChannel("testing")

	if got := mr.HGet("ota", "preview-status:mdb"); got != status.PreviewReady {
		t.Errorf("preview-status:mdb = %q, want %q", got, status.PreviewReady)
	}
	if got := mr.HGet("ota", "preview-version:mdb"); got != "testing-20260920T211120" {
		t.Errorf("preview-version:mdb = %q, want the testing release from the channel list", got)
	}
}

// A channel the manifest and the channel list both lack has nothing to switch
// to, which is not an error the UI should retry.
func TestPreviewChannel_UnavailableForChannelMissingFromManifest(t *testing.T) {
	u, mr := newTestUpdaterForPreview(t, stableIndex())
	mr.HSet("version:mdb", "variant_id", "unu-mdb")

	u.previewChannel("testing")

	if got := mr.HGet("ota", "preview-status:mdb"); got != status.PreviewUnavailable {
		t.Errorf("preview-status:mdb = %q, want %q", got, status.PreviewUnavailable)
	}
}

// An unreachable index must not leave the UI waiting on "checking" forever.
func TestPreviewChannel_ErrorOnUnreachableIndex(t *testing.T) {
	u, mr := newTestUpdaterForPreview(t, nil)
	mr.HSet("version:mdb", "variant_id", "unu-mdb")
	// previewChannel derives its deadline from u.ctx, so a short parent
	// stands in for previewTimeout without making the test wait for it.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	u.ctx = ctx

	u.previewChannel("testing")

	if got := mr.HGet("ota", "preview-status:mdb"); got != status.PreviewError {
		t.Errorf("preview-status:mdb = %q, want %q", got, status.PreviewError)
	}
	if got := mr.HGet("ota", "preview-channel:mdb"); got != "testing" {
		t.Errorf("preview-channel:mdb = %q, want testing", got)
	}
}

func TestPreviewChannel_RejectsInvalidChannel(t *testing.T) {
	u, mr := newTestUpdaterForPreview(t, stableIndex())
	mr.HSet("version:mdb", "variant_id", "unu-mdb")

	u.previewChannel("bogus")

	if got := mr.HGet("ota", "preview-status:mdb"); got != status.PreviewError {
		t.Errorf("preview-status:mdb = %q, want %q", got, status.PreviewError)
	}
}

// A preview must not disturb an update already in flight: it writes preview-*
// fields and nothing else.
func TestPreviewChannel_LeavesUpdateStatusAlone(t *testing.T) {
	u, mr := newTestUpdaterForPreview(t, stableIndex())
	mr.HSet("version:mdb", "variant_id", "unu-mdb")

	if err := u.status.SetDownloading(u.ctx, "nightly-20260101T000000", "delta"); err != nil {
		t.Fatal(err)
	}

	u.previewChannel("stable")

	if got := mr.HGet("ota", "status:mdb"); got != string(status.StatusDownloading) {
		t.Errorf("status:mdb = %q, want downloading", got)
	}
	if got := mr.HGet("ota", "update-version:mdb"); got != "nightly-20260101T000000" {
		t.Errorf("update-version:mdb = %q, want the in-flight target", got)
	}
}
