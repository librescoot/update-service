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
	"time"
)

func TestDownloader_SkipsCompleteFile(t *testing.T) {
	// Create a test server that serves a file
	content := []byte("test file content for download")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
			return
		}
		_, _ = w.Write(content)
	}))
	defer server.Close()

	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "download_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	logger := log.New(os.Stdout, "test: ", 0)
	downloader := NewDownloader(tmpDir, Budget{}, logger)

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
			_, _ = fmt.Sscanf(rangeHeader, "bytes=%d-", &start)
			w.Header().Set("Content-Length", fmt.Sprintf("%d", int64(len(content))-start))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(content[start:])
			return
		}

		_, _ = w.Write(content)
	}))
	defer server.Close()

	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "download_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	logger := log.New(os.Stdout, "test: ", 0)
	downloader := NewDownloader(tmpDir, Budget{}, logger)

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
		_, _ = w.Write(content)
	}))
	defer server.Close()

	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "download_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	logger := log.New(os.Stdout, "test: ", 0)
	downloader := NewDownloader(tmpDir, Budget{}, logger)

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
			_, _ = fmt.Sscanf(rangeHeader, "bytes=%d-", &start)
			if start >= int64(len(content)) {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(content)))
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			w.Header().Set("Content-Length", fmt.Sprintf("%d", int64(len(content))-start))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(content[start:])
			return
		}
		_, _ = w.Write(content)
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
	downloader := NewDownloader(tmpDir, Budget{}, logger)

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
	downloader := NewDownloader(tmpDir, Budget{}, logger)

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
			_, _ = fmt.Sscanf(rangeHeader, "bytes=%d-", &start)
			if start >= int64(len(content)) {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			w.Header().Set("Content-Length", fmt.Sprintf("%d", int64(len(content))-start))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(content[start:])
			return
		}
		_, _ = w.Write(content)
	}))
	defer server.Close()

	tmpDir, err := os.MkdirTemp("", "download_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	logger := log.New(os.Stdout, "test: ", 0)
	downloader := NewDownloader(tmpDir, Budget{}, logger)

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
	downloader := NewDownloader(tmpDir, Budget{}, logger)

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
	downloader := NewDownloader(tmpDir, Budget{}, logger)

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
	downloader := NewDownloader(tmpDir, Budget{}, logger)

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
	downloader := NewDownloader(tmpDir, Budget{}, logger)

	size, err := downloader.getExpectedFileSize(context.Background(), server.URL+"/test.mender")
	if err != nil {
		t.Fatalf("getExpectedFileSize failed: %v", err)
	}

	if size != int64(len(content)) {
		t.Errorf("Expected size %d, got %d", len(content), size)
	}
}

// trickleServer serves content at a fixed byte rate, so a test can drive the
// throughput floor deterministically without sleeping for real minutes.
func trickleServer(content []byte, chunk int, gap time.Duration) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for off := 0; off < len(content); off += chunk {
			end := min(off+chunk, len(content))
			if _, err := w.Write(content[off:end]); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(gap)
		}
	}))
}

func TestDownloader_AbortsWhenBelowThroughputFloor(t *testing.T) {
	content := make([]byte, 64*1024)
	// 64 bytes every 50ms is ~1.3 kB/s, far under a 4 KB per 200ms floor.
	server := trickleServer(content, 64, 50*time.Millisecond)
	defer server.Close()

	tmpDir := t.TempDir()
	logger := log.New(os.Stdout, "test: ", 0)
	budget := Budget{StallWindow: 200 * time.Millisecond, StallMinBytes: 4096}
	d := NewDownloader(tmpDir, budget, logger)

	_, err := d.Download(context.Background(), server.URL+"/slow.mender", nil)
	if !errors.Is(err, ErrDownloadStalled) {
		t.Fatalf("expected ErrDownloadStalled, got %v", err)
	}

	// The partial must survive for the next attempt to resume from.
	info, statErr := os.Stat(filepath.Join(tmpDir, "slow.mender.tmp"))
	if statErr != nil {
		t.Fatalf("expected partial .tmp to survive abort: %v", statErr)
	}
	if info.Size() == 0 {
		t.Errorf("expected partial to hold the bytes received before abort")
	}
}

func TestDownloader_AbortsOnWallClockBudget(t *testing.T) {
	content := make([]byte, 512*1024)
	// Fast enough to clear any floor, but longer than MaxDuration overall.
	server := trickleServer(content, 8*1024, 20*time.Millisecond)
	defer server.Close()

	tmpDir := t.TempDir()
	logger := log.New(os.Stdout, "test: ", 0)
	budget := Budget{
		MaxDuration:   150 * time.Millisecond,
		StallWindow:   10 * time.Second,
		StallMinBytes: 1,
	}
	d := NewDownloader(tmpDir, budget, logger)

	_, err := d.Download(context.Background(), server.URL+"/big.mender", nil)
	if !errors.Is(err, ErrDownloadBudgetExceeded) {
		t.Fatalf("expected ErrDownloadBudgetExceeded, got %v", err)
	}
}

func TestDownloader_AbortsSilentServerAtStallWindow(t *testing.T) {
	// Accepts, sends headers, then never writes a byte. Without the stall
	// timer this blocks until the kernel gives up.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", "1048576")
			return
		}
		w.Header().Set("Content-Length", "1048576")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(5 * time.Second)
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	logger := log.New(os.Stdout, "test: ", 0)
	d := NewDownloader(tmpDir, Budget{StallWindow: 200 * time.Millisecond, StallMinBytes: 1}, logger)

	start := time.Now()
	_, err := d.Download(context.Background(), server.URL+"/silent.mender", nil)
	if !errors.Is(err, ErrDownloadStalled) {
		t.Fatalf("expected ErrDownloadStalled, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("stall abort took %v, expected roughly StallWindow", elapsed)
	}
}

func TestDownloader_ParentCancelIsNotABudgetAbort(t *testing.T) {
	content := make([]byte, 256*1024)
	server := trickleServer(content, 4*1024, 20*time.Millisecond)
	defer server.Close()

	tmpDir := t.TempDir()
	logger := log.New(os.Stdout, "test: ", 0)
	d := NewDownloader(tmpDir, Budget{StallWindow: 10 * time.Second, StallMinBytes: 1}, logger)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	_, err := d.Download(ctx, server.URL+"/shutdown.mender", nil)
	if errors.Is(err, ErrDownloadStalled) || errors.Is(err, ErrDownloadBudgetExceeded) {
		t.Fatalf("shutdown must not be reported as a budget abort, got %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestDownloader_BurstThenShortGapDoesNotAbort(t *testing.T) {
	// One big burst clears the floor, then a gap shorter than StallWindow,
	// then the rest. A healthy link with server-side buffering looks like this.
	content := make([]byte, 32*1024)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		w.WriteHeader(http.StatusOK)
		f, _ := w.(http.Flusher)
		_, _ = w.Write(content[:16*1024])
		if f != nil {
			f.Flush()
		}
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write(content[16*1024:])
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	logger := log.New(os.Stdout, "test: ", 0)
	d := NewDownloader(tmpDir, Budget{StallWindow: time.Second, StallMinBytes: 8 * 1024}, logger)

	path, err := d.Download(context.Background(), server.URL+"/bursty.mender", nil)
	if err != nil {
		t.Fatalf("healthy bursty download must not abort: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != int64(len(content)) {
		t.Fatalf("expected complete file of %d bytes, got %v (%v)", len(content), info, err)
	}
}

func TestDownloader_ZeroBudgetIsUnlimited(t *testing.T) {
	content := make([]byte, 8*1024)
	server := trickleServer(content, 256, 10*time.Millisecond)
	defer server.Close()

	tmpDir := t.TempDir()
	logger := log.New(os.Stdout, "test: ", 0)
	d := NewDownloader(tmpDir, Budget{}, logger)

	if _, err := d.Download(context.Background(), server.URL+"/unlimited.mender", nil); err != nil {
		t.Fatalf("zero budget must impose no limits, got %v", err)
	}
}
