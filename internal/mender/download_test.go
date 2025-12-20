package mender

import (
	"context"
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
