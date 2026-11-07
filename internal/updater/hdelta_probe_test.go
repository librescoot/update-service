package updater

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHdeltaProbe(t *testing.T) {
	u, mr := newTestUpdaterForPreview(t, nil)
	nonce := strings.Repeat("a", 32)
	key := "ota:hdelta-probe:mdb:" + nonce
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	u.handleCommand("probe-hdelta:" + nonce)
	if got, _ := mr.Get(key); got != "0" {
		t.Fatalf("missing decoder: %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "hpatchz"), []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	u.handleCommand("probe-hdelta:" + nonce)
	if got, _ := mr.Get(key); got != "1" {
		t.Fatalf("available decoder: %q", got)
	}
	if mr.TTL(key) <= 0 {
		t.Fatal("probe response has no expiry")
	}
	u.handleCommand("probe-hdelta:../../bad")
	if mr.Exists("ota:hdelta-probe:mdb:../../bad") {
		t.Fatal("invalid nonce accepted")
	}
}
