package mender

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func sha256Of(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// serveBytes serves whatever the current payload is, answering HEAD with its
// size, so a test can swap corrupt bytes for good ones between attempts.
func serveBytes(t *testing.T, payload *atomic.Value, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		content := payload.Load().([]byte)
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
			return
		}
		hits.Add(1)
		_, _ = w.Write(content)
	}))
}

// A delta that fails verification must not be left on disk: Download
// short-circuits on a size match, so the next attempt would re-verify the same
// bad bytes instead of re-fetching, and the retry budget would expire without
// a single byte having been downloaded again.
func TestManager_DownloadDelta_DiscardsUnverifiedFile(t *testing.T) {
	good := []byte("the delta bytes the release channel signed")
	bad := make([]byte, len(good))
	copy(bad, good)
	bad[0] ^= 0xff

	var payload atomic.Value
	payload.Store(bad)
	var hits atomic.Int32
	server := serveBytes(t, &payload, &hits)
	defer server.Close()

	tmpDir := t.TempDir()
	m := NewManager(tmpDir, log.New(io.Discard, "", 0))
	url := server.URL + "/update.delta"
	deltaPath := filepath.Join(tmpDir, "update.delta")

	_, err := m.DownloadDelta(context.Background(), url, sha256Of(good), nil)
	if err == nil {
		t.Fatal("expected checksum verification to fail")
	}
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("expected ErrChecksumMismatch, got %v", err)
	}
	if _, statErr := os.Stat(deltaPath); !os.IsNotExist(statErr) {
		t.Fatalf("unverified delta was left on disk: %v", statErr)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("expected 1 GET, got %d", got)
	}

	// Second attempt: the host now serves the good bytes. With the bad file
	// gone, the retry must actually re-fetch rather than short-circuit on size.
	payload.Store(good)
	got, err := m.DownloadDelta(context.Background(), url, sha256Of(good), nil)
	if err != nil {
		t.Fatalf("retry after discard failed: %v", err)
	}
	if got != deltaPath {
		t.Fatalf("expected %s, got %s", deltaPath, got)
	}
	if hits.Load() != 2 {
		t.Fatalf("retry did not re-download, GET count is %d", hits.Load())
	}
	data, err := os.ReadFile(deltaPath)
	if err != nil {
		t.Fatalf("reading re-downloaded delta: %v", err)
	}
	if string(data) != string(good) {
		t.Fatal("re-downloaded delta does not hold the verified bytes")
	}
}

// The full-artifact path has the same short-circuit, so it needs the same
// discard.
func TestManager_DownloadAndVerify_DiscardsUnverifiedFile(t *testing.T) {
	good := []byte("the mender artifact the release channel signed")
	bad := make([]byte, len(good))
	copy(bad, good)
	bad[len(bad)-1] ^= 0xff

	var payload atomic.Value
	payload.Store(bad)
	var hits atomic.Int32
	server := serveBytes(t, &payload, &hits)
	defer server.Close()

	tmpDir := t.TempDir()
	m := NewManager(tmpDir, log.New(io.Discard, "", 0))
	url := server.URL + "/update.mender"
	menderPath := filepath.Join(tmpDir, "update.mender")

	if _, err := m.DownloadAndVerify(context.Background(), url, sha256Of(good), nil); err == nil {
		t.Fatal("expected checksum verification to fail")
	}
	if _, statErr := os.Stat(menderPath); !os.IsNotExist(statErr) {
		t.Fatalf("unverified artifact was left on disk: %v", statErr)
	}

	payload.Store(good)
	if _, err := m.DownloadAndVerify(context.Background(), url, sha256Of(good), nil); err != nil {
		t.Fatalf("retry after discard failed: %v", err)
	}
	if hits.Load() != 2 {
		t.Fatalf("retry did not re-download, GET count is %d", hits.Load())
	}
}

// Without the discard, a size match is enough to skip the transfer entirely -
// the behaviour that turns a bad file into a permanently stuck retry loop.
func TestDownloader_SkipsOnSizeMatchAlone(t *testing.T) {
	good := []byte("payload of a known size")
	bad := make([]byte, len(good))
	copy(bad, good)
	bad[3] ^= 0xff

	var payload atomic.Value
	payload.Store(good)
	var hits atomic.Int32
	server := serveBytes(t, &payload, &hits)
	defer server.Close()

	tmpDir := t.TempDir()
	d := NewDownloader(tmpDir, log.New(io.Discard, "", 0))
	filePath := filepath.Join(tmpDir, "update.delta")
	if err := os.WriteFile(filePath, bad, 0644); err != nil {
		t.Fatalf("seeding corrupt file: %v", err)
	}

	if _, err := d.Download(context.Background(), server.URL+"/update.delta", nil); err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("expected the transfer to be skipped, GET count is %d", hits.Load())
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("reading file: %v", err)
	}
	if string(data) != string(bad) {
		t.Fatal("expected the corrupt bytes to survive; Download does not verify content")
	}
}
