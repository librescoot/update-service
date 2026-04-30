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

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		// Semver: same channel, v-prefixed dotted — must NOT lex-compare.
		{"stable-v0.7.0", "stable-v0.10.0", -1},
		{"stable-v0.10.0", "stable-v0.7.0", 1},
		{"stable-v1.0.0", "stable-v0.99.99", 1},
		{"stable-v0.8.0", "stable-v0.8.0", 0},

		// Timestamp tokens: lex compare is correct.
		{"nightly-20260101T120000", "nightly-20260415T120000", -1},
		{"nightly-20260415T120000", "nightly-20260101T120000", 1},

		// Cross-channel: falls back to lex (stable across runs is enough).
		{"nightly-20260101T120000", "stable-v0.7.0", -1},
		{"stable-v0.7.0", "nightly-20260101T120000", 1},
	}
	for _, c := range cases {
		got := compareVersions(c.a, c.b)
		if got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestManager_CleanupStaleMenderFiles_SemverAware(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "mender_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	logger := log.New(os.Stdout, "test: ", 0)
	manager := NewManager(tmpDir, logger)

	// v0.10.0 is the newest — lex compare would (wrongly) keep v0.7.0.
	files := []string{
		"librescoot-unu-mdb-stable-v0.7.0.mender",
		"librescoot-unu-mdb-stable-v0.8.0.mender",
		"librescoot-unu-mdb-stable-v0.10.0.mender",
	}
	for _, f := range files {
		p := filepath.Join(tmpDir, f)
		if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
			t.Fatalf("Failed to create %s: %v", f, err)
		}
	}

	manager.CleanupStaleMenderFiles()

	kept := filepath.Join(tmpDir, "librescoot-unu-mdb-stable-v0.10.0.mender")
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("expected v0.10.0 to be kept, got error: %v", err)
	}
	for _, f := range []string{
		"librescoot-unu-mdb-stable-v0.7.0.mender",
		"librescoot-unu-mdb-stable-v0.8.0.mender",
	} {
		p := filepath.Join(tmpDir, f)
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("expected %s to be removed, got err=%v", f, err)
		}
	}
}
