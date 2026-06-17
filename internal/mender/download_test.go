package mender

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestDownloader_SkipsCompleteFile(t *testing.T) {
	// Create a test server that serves a file
	content := []byte("test file content for download")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
			return
		}
		w.Write(content)
	}))
	defer server.Close()

	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "download_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	logger := log.New(os.Stdout, "test: ", 0)
	downloader := NewDownloader(tmpDir, logger)

	// Pre-create a complete file
	filename := "testfile.mender"
	filePath := filepath.Join(tmpDir, filename)
	if err := os.WriteFile(filePath, content, 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// Download should skip and return existing file
	result, err := downloader.Download(context.Background(), server.URL+"/"+filename, nil)
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}

	if result != filePath {
		t.Errorf("Expected %s, got %s", filePath, result)
	}

	// Verify file wasn't modified
	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("Failed to read file: %v", err)
	}
	if string(data) != string(content) {
		t.Errorf("File content changed unexpectedly")
	}
}

func TestDownloader_ResumesIncompleteFile(t *testing.T) {
	// Create a test server that serves a file and supports range requests
	content := []byte("this is the complete file content for testing resume")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
			return
		}

		// Check for Range header
		rangeHeader := r.Header.Get("Range")
		if rangeHeader != "" {
			var start int64
			fmt.Sscanf(rangeHeader, "bytes=%d-", &start)
			w.Header().Set("Content-Length", fmt.Sprintf("%d", int64(len(content))-start))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(content[start:])
			return
		}

		w.Write(content)
	}))
	defer server.Close()

	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "download_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	logger := log.New(os.Stdout, "test: ", 0)
	downloader := NewDownloader(tmpDir, logger)

	// Pre-create an incomplete file (first half of content)
	filename := "testfile.mender"
	filePath := filepath.Join(tmpDir, filename)
	partialContent := content[:len(content)/2]
	if err := os.WriteFile(filePath, partialContent, 0644); err != nil {
		t.Fatalf("Failed to create partial file: %v", err)
	}

	// Download should detect incomplete file and resume
	result, err := downloader.Download(context.Background(), server.URL+"/"+filename, nil)
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}

	if result != filePath {
		t.Errorf("Expected %s, got %s", filePath, result)
	}

	// Verify file is now complete
	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("Failed to read file: %v", err)
	}
	if string(data) != string(content) {
		t.Errorf("File content mismatch: got %q, want %q", string(data), string(content))
	}
}

func TestDownloader_DeletesOversizedFile(t *testing.T) {
	// Create a test server
	content := []byte("short content")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
			return
		}
		w.Write(content)
	}))
	defer server.Close()

	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "download_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	logger := log.New(os.Stdout, "test: ", 0)
	downloader := NewDownloader(tmpDir, logger)

	// Pre-create an oversized file (larger than server content)
	filename := "testfile.mender"
	filePath := filepath.Join(tmpDir, filename)
	oversizedContent := []byte("this content is much longer than what the server will serve")
	if err := os.WriteFile(filePath, oversizedContent, 0644); err != nil {
		t.Fatalf("Failed to create oversized file: %v", err)
	}

	// Download should detect corruption and re-download
	result, err := downloader.Download(context.Background(), server.URL+"/"+filename, nil)
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}

	if result != filePath {
		t.Errorf("Expected %s, got %s", filePath, result)
	}

	// Verify file has correct content now
	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("Failed to read file: %v", err)
	}
	if string(data) != string(content) {
		t.Errorf("File content mismatch: got %q, want %q", string(data), string(content))
	}
}

// rangeAware416Server serves content with realistic Range semantics: a HEAD
// returns Content-Length, a satisfiable Range returns 206, and an unsatisfiable
// Range (start >= length) returns 416 with a Content-Range total, the way S3 /
// GitHub release storage does.
func rangeAware416Server(t *testing.T, content []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
			return
		}
		if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
			var start int64
			fmt.Sscanf(rangeHeader, "bytes=%d-", &start)
			if start >= int64(len(content)) {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(content)))
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			w.Header().Set("Content-Length", fmt.Sprintf("%d", int64(len(content))-start))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(content[start:])
			return
		}
		w.Write(content)
	}))
}

// A .tmp left at full size (download wrote every byte but the process died before
// the rename) must be finalized, not abandoned. Resuming it sends an unsatisfiable
// Range and the server answers 416; the downloader has to treat that as "already
// complete" rather than a fatal error.
func TestDownloader_FinalizesCompleteTmpOn416(t *testing.T) {
	content := []byte("this is the complete file content for the 416 resume case")
	server := rangeAware416Server(t, content)
	defer server.Close()

	tmpDir, err := os.MkdirTemp("", "download_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	logger := log.New(os.Stdout, "test: ", 0)
	downloader := NewDownloader(tmpDir, logger)

	filename := "testfile.mender"
	filePath := filepath.Join(tmpDir, filename)
	tmpPath := filePath + ".tmp"
	if err := os.WriteFile(tmpPath, content, 0644); err != nil {
		t.Fatalf("Failed to create complete tmp file: %v", err)
	}

	result, err := downloader.Download(context.Background(), server.URL+"/"+filename, nil)
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	if result != filePath {
		t.Errorf("Expected %s, got %s", filePath, result)
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("Failed to read file: %v", err)
	}
	if string(data) != string(content) {
		t.Errorf("File content mismatch: got %q, want %q", string(data), string(content))
	}
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Errorf("Expected .tmp to be renamed to final, but it still exists")
	}
}

// A .tmp larger than the current server content (the asset was re-published smaller
// under the same name) is stale: resuming sends an unsatisfiable Range -> 416. The
// downloader must discard it and re-download fresh rather than failing.
func TestDownloader_DiscardsOversizedTmpOn416(t *testing.T) {
	content := []byte("short final content")
	server := rangeAware416Server(t, content)
	defer server.Close()

	tmpDir, err := os.MkdirTemp("", "download_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	logger := log.New(os.Stdout, "test: ", 0)
	downloader := NewDownloader(tmpDir, logger)

	filename := "testfile.mender"
	filePath := filepath.Join(tmpDir, filename)
	tmpPath := filePath + ".tmp"
	oversized := []byte("this stale partial is much longer than the current server content")
	if err := os.WriteFile(tmpPath, oversized, 0644); err != nil {
		t.Fatalf("Failed to create oversized tmp file: %v", err)
	}

	result, err := downloader.Download(context.Background(), server.URL+"/"+filename, nil)
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	if result != filePath {
		t.Errorf("Expected %s, got %s", filePath, result)
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("Failed to read file: %v", err)
	}
	if string(data) != string(content) {
		t.Errorf("File content mismatch: got %q, want %q", string(data), string(content))
	}
}

// When the server won't answer a HEAD with a usable Content-Length, the proactive
// size check can't run, so a complete .tmp still triggers a 416 on resume. The
// defensive 416 branch must finalize it rather than failing.
func TestDownloader_Finalizes416WhenHeadUnavailable(t *testing.T) {
	content := []byte("complete partial, but the server hides its size from HEAD")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			// No Content-Length: getExpectedFileSize fails, skipping the proactive check.
			return
		}
		if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
			var start int64
			fmt.Sscanf(rangeHeader, "bytes=%d-", &start)
			if start >= int64(len(content)) {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			w.Header().Set("Content-Length", fmt.Sprintf("%d", int64(len(content))-start))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(content[start:])
			return
		}
		w.Write(content)
	}))
	defer server.Close()

	tmpDir, err := os.MkdirTemp("", "download_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	logger := log.New(os.Stdout, "test: ", 0)
	downloader := NewDownloader(tmpDir, logger)

	filename := "testfile.mender"
	filePath := filepath.Join(tmpDir, filename)
	tmpPath := filePath + ".tmp"
	if err := os.WriteFile(tmpPath, content, 0644); err != nil {
		t.Fatalf("Failed to create complete tmp file: %v", err)
	}

	result, err := downloader.Download(context.Background(), server.URL+"/"+filename, nil)
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	if result != filePath {
		t.Errorf("Expected %s, got %s", filePath, result)
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("Failed to read file: %v", err)
	}
	if string(data) != string(content) {
		t.Errorf("File content mismatch: got %q, want %q", string(data), string(content))
	}
}

// A pruned release asset (gone from the download host) must surface as
// ErrAssetUnavailable so the delta retry loop can fall back to a full update
// immediately instead of spinning on a permanent error.
func TestDownloader_ReportsAssetUnavailableOn404(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	tmpDir, err := os.MkdirTemp("", "download_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	logger := log.New(os.Stdout, "test: ", 0)
	downloader := NewDownloader(tmpDir, logger)

	_, err = downloader.Download(context.Background(), server.URL+"/gone.delta", nil)
	if !errors.Is(err, ErrAssetUnavailable) {
		t.Fatalf("Expected ErrAssetUnavailable, got %v", err)
	}
}

func TestDownloader_ReportsAssetUnavailableOn410(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGone)
	}))
	defer server.Close()

	tmpDir, err := os.MkdirTemp("", "download_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	logger := log.New(os.Stdout, "test: ", 0)
	downloader := NewDownloader(tmpDir, logger)

	_, err = downloader.Download(context.Background(), server.URL+"/gone.delta", nil)
	if !errors.Is(err, ErrAssetUnavailable) {
		t.Fatalf("Expected ErrAssetUnavailable, got %v", err)
	}
}

// Resuming a partial whose release was pruned must also report ErrAssetUnavailable
// rather than hanging or falsely finalizing the stale partial.
func TestDownloader_ReportsAssetUnavailableOnResume(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	tmpDir, err := os.MkdirTemp("", "download_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	logger := log.New(os.Stdout, "test: ", 0)
	downloader := NewDownloader(tmpDir, logger)

	filename := "gone.delta"
	tmpPath := filepath.Join(tmpDir, filename+".tmp")
	if err := os.WriteFile(tmpPath, []byte("partial bytes from a now-deleted release"), 0644); err != nil {
		t.Fatalf("Failed to create partial file: %v", err)
	}

	_, err = downloader.Download(context.Background(), server.URL+"/"+filename, nil)
	if !errors.Is(err, ErrAssetUnavailable) {
		t.Fatalf("Expected ErrAssetUnavailable, got %v", err)
	}
}

func TestGetExpectedFileSize(t *testing.T) {
	content := []byte("test content for size check")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("Expected HEAD request, got %s", r.Method)
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
	}))
	defer server.Close()

	tmpDir, err := os.MkdirTemp("", "download_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	logger := log.New(os.Stdout, "test: ", 0)
	downloader := NewDownloader(tmpDir, logger)

	size, err := downloader.getExpectedFileSize(context.Background(), server.URL+"/test.mender")
	if err != nil {
		t.Fatalf("getExpectedFileSize failed: %v", err)
	}

	if size != int64(len(content)) {
		t.Errorf("Expected size %d, got %d", len(content), size)
	}
}
