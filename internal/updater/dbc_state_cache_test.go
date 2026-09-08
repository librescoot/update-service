package updater

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"github.com/librescoot/update-service/internal/dbcstate"
	"github.com/librescoot/update-service/internal/redis"
)

func newDBCStateCacheUpdater(t *testing.T) (*Updater, *miniredis.Miniredis, string) {
	t.Helper()
	mr := miniredis.RunT(t)
	rc, err := redis.New(mr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rc.Close() })
	path := filepath.Join(t.TempDir(), "dbc-state.json")
	return &Updater{
		redis:         rc,
		dbcStateCache: path,
		logger:        log.New(io.Discard, "", 0),
		ctx:           context.Background(),
	}, mr, path
}

func TestRestoreDBCStateCache(t *testing.T) {
	u, mr, path := newDBCStateCacheUpdater(t)
	if err := dbcstate.Save(path, dbcstate.Snapshot{
		RunningVersion: "v1.3.1",
		TargetVersion:  "v1.4.0",
		Status:         "pending-reboot",
	}); err != nil {
		t.Fatal(err)
	}

	u.restoreDBCStateCache()
	if got := mr.HGet("version:dbc", "version_id"); got != "v1.3.1" {
		t.Fatalf("running version = %q", got)
	}
	if got := mr.HGet("ota", "update-version:dbc"); got != "v1.4.0" {
		t.Fatalf("target version = %q", got)
	}
	if got := mr.HGet("ota", "status:dbc"); got != "pending-reboot" {
		t.Fatalf("status = %q", got)
	}
	if got := mr.HGet("ota", "state-origin:dbc"); got != "cached" {
		t.Fatalf("origin = %q", got)
	}
}

func TestPersistDBCStateCacheSkipsRestoredFacts(t *testing.T) {
	u, mr, path := newDBCStateCacheUpdater(t)
	mr.HSet("version:dbc", "version_id", "v1.3.1")
	mr.HSet("ota", "status:dbc", "idle")
	mr.HSet("ota", "state-origin:dbc", "cached")

	u.persistDBCStateCache()
	if _, err := dbcstate.Load(path); err == nil {
		t.Fatal("restored cache was written back as a live observation")
	}
}

func TestPersistLiveDBCStateCache(t *testing.T) {
	u, mr, path := newDBCStateCacheUpdater(t)
	mr.HSet("version:dbc", "version_id", "v1.4.0")
	mr.HSet("ota", "status:dbc", "idle")
	mr.HSet("ota", "state-origin:dbc", "live")

	u.persistDBCStateCache()
	got, err := dbcstate.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.RunningVersion != "v1.4.0" || got.Status != "idle" {
		t.Fatalf("unexpected snapshot: %+v", got)
	}
}
