package mender

import (
	"log"
	"os"
	"path/filepath"
	"testing"
)

func TestManager_RemoveFile(t *testing.T) {
	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "mender_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	logger := log.New(os.Stdout, "test: ", 0)
	manager := NewManager(tmpDir, logger)

	// Create a file to remove
	testFile := filepath.Join(tmpDir, "test.mender")
	if err := os.WriteFile(testFile, []byte("test content"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// Remove the file
	err = manager.RemoveFile(testFile)
	if err != nil {
		t.Errorf("RemoveFile failed: %v", err)
	}

	// Verify file is gone
	if _, err := os.Stat(testFile); !os.IsNotExist(err) {
		t.Errorf("File should have been removed")
	}
}

func TestManager_RemoveFile_RefusesOutsideDir(t *testing.T) {
	// Create temp directories
	tmpDir, err := os.MkdirTemp("", "mender_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	otherDir, err := os.MkdirTemp("", "other_test")
	if err != nil {
		t.Fatalf("Failed to create other dir: %v", err)
	}
	defer os.RemoveAll(otherDir)

	logger := log.New(os.Stdout, "test: ", 0)
	manager := NewManager(tmpDir, logger)

	// Create a file outside the download directory
	outsideFile := filepath.Join(otherDir, "outside.mender")
	if err := os.WriteFile(outsideFile, []byte("test content"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// Attempt to remove file outside download directory should fail
	err = manager.RemoveFile(outsideFile)
	if err == nil {
		t.Errorf("RemoveFile should have refused to remove file outside download directory")
	}

	// Verify file still exists
	if _, err := os.Stat(outsideFile); os.IsNotExist(err) {
		t.Errorf("File outside download directory should not have been removed")
	}
}

func TestManager_FindMenderFileForVersion(t *testing.T) {
	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "mender_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	logger := log.New(os.Stdout, "test: ", 0)
	manager := NewManager(tmpDir, logger)

	// Create a mender file with version in name
	testFile := filepath.Join(tmpDir, "librescoot-unu-dbc-nightly-20251212T024719.mender")
	if err := os.WriteFile(testFile, []byte("test content"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// Should find file by version
	path, found := manager.FindMenderFileForVersion("nightly-20251212T024719")
	if !found {
		t.Errorf("Should have found mender file for version")
	}
	if path != testFile {
		t.Errorf("Expected %s, got %s", testFile, path)
	}

	// Should find with lowercase version
	path, found = manager.FindMenderFileForVersion("nightly-20251212t024719")
	if !found {
		t.Errorf("Should have found mender file for lowercase version")
	}

	// Should not find non-existent version
	_, found = manager.FindMenderFileForVersion("nightly-99999999T999999")
	if found {
		t.Errorf("Should not have found mender file for non-existent version")
	}
}
