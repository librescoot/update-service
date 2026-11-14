package updater

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/librescoot/update-service/internal/dbcstate"
	"github.com/librescoot/update-service/internal/mender"
)

func TestOfficialFeedRequiresCommittedBridge(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Updater)
		wantV2 bool
	}{
		{"committed bridge", func(*Updater) {}, true},
		{"marker missing", func(u *Updater) { u.readV2Marker = func() ([]byte, error) { return nil, os.ErrNotExist } }, false},
		{"marker invalid", func(u *Updater) { u.readV2Marker = func() ([]byte, error) { return []byte("v3\n"), nil } }, false},
		{"marker wrong image", func(u *Updater) {
			u.readV2Marker = func() ([]byte, error) { return []byte("v2\nrelease-v1.4.4\n"), nil }
		}, false},
		{"decoder missing", func(u *Updater) { u.checkV2Decoder = func() error { return errors.New("missing zstd") } }, false},
		{"Mender unavailable", func(u *Updater) {
			u.observeUpdate = func() (mender.UpdateObservation, error) { return mender.UpdateObservation{}, errors.New("unavailable") }
		}, false},
		{"pending commit", func(u *Updater) {
			u.observeUpdate = func() (mender.UpdateObservation, error) {
				return mender.UpdateObservation{State: mender.StateNeedsCommit, CommittedArtifact: "release-v1.4.3", CommittedVersion: "v1.4.3", PendingArtifact: "release-v1.5.0"}, nil
			}
		}, false},
		{"wrong running rootfs", func(u *Updater) { u.runningVersion = func() (string, error) { return "v1.4.2", nil } }, false},
		{"unreadable rootfs", func(u *Updater) { u.runningVersion = func() (string, error) { return "", errors.New("unavailable") } }, false},
		{"unresolved marker", func(u *Updater) {
			_ = dbcstate.SaveActivationAttempt(u.activationAttempt, dbcstate.ActivationAttempt{Artifact: "release-v1.5.0", BootID: "boot-a"})
		}, false},
	}
	for _, component := range []string{"mdb", "dbc"} {
		for _, tc := range cases {
			t.Run(component+"/"+tc.name, func(t *testing.T) {
				u, _ := newPendingCommitUpdater(t)
				u.config.Component = component
				u.readV2Marker = func() ([]byte, error) { return []byte("v2\nrelease-v1.4.3\n"), nil }
				u.checkV2Decoder = func() error { return nil }
				u.runningVersion = func() (string, error) { return "v1.4.3", nil }
				u.observeUpdate = func() (mender.UpdateObservation, error) {
					return mender.UpdateObservation{State: mender.StateNoUpdate, CommittedArtifact: "release-v1.4.3", CommittedVersion: "v1.4.3"}, nil
				}
				tc.change(u)
				got := u.releaseFeedBase(defaultReleaseFeed)
				want := defaultReleaseFeed
				if tc.wantV2 {
					want += "/v2"
				}
				if got != want {
					t.Fatalf("feed=%s, want %s", got, want)
				}
				if got := u.releaseFeedBase("https://private.example/releases"); got != "https://private.example/releases" {
					t.Fatalf("custom feed changed: %s", got)
				}
			})
		}
	}
}

func TestMissingV2FallsBackToLegacy(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/v2/latest.json" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"stable":{"tag_name":"v1.4.3"}}`))
	}))
	defer srv.Close()
	u, _ := newPendingCommitUpdater(t)
	api := NewGitHubAPI(u.ctx, srv.URL, u.logger)
	api.selectBaseURL = func(base string) string { return base + "/v2" }
	got, err := api.GetLatestByChannel()
	if err != nil || got["stable"].TagName != "v1.4.3" {
		t.Fatalf("legacy fallback %v %v", got, err)
	}
	if len(paths) != 2 || paths[0] != "/v2/latest.json" || paths[1] != "/latest.json" {
		t.Fatal(paths)
	}
}

func TestFeedSelectionAtEachRequest(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v2/stable.json" {
			_, _ = w.Write([]byte("[]"))
			return
		}
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()
	u, _ := newPendingCommitUpdater(t)
	u.activationAttempt = filepath.Join(t.TempDir(), "attempt")
	u.readV2Marker = func() ([]byte, error) { return []byte("v2\nrelease-v1.4.3\n"), nil }
	u.checkV2Decoder = func() error { return nil }
	staged := true
	u.observeUpdate = func() (mender.UpdateObservation, error) {
		o := mender.UpdateObservation{State: mender.StateNoUpdate, CommittedArtifact: "release-v1.4.3", CommittedVersion: "v1.4.3"}
		if staged {
			o.State = mender.StateNeedsCommit
			o.PendingArtifact = "release-v1.5.0"
		}
		return o, nil
	}
	u.runningVersion = func() (string, error) { return "v1.4.3", nil }
	api := NewGitHubAPI(u.ctx, srv.URL, u.logger)
	api.selectBaseURL = func(base string) string {
		if u.releaseFeedBase(defaultReleaseFeed) == defaultReleaseFeed+"/v2" {
			return base + "/v2"
		}
		return base
	}
	if _, err := api.GetLatestByChannel(); err != nil {
		t.Fatal(err)
	}
	staged = false
	if _, err := api.GetReleases("stable"); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || paths[0] != "/latest.json" || paths[1] != "/v2/stable.json" {
		t.Fatal(paths)
	}
}
