package delta

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestApplyHDiffReal(t *testing.T) {
	hdiff := os.Getenv("HDIFFZ")
	if hdiff == "" {
		t.Skip("set HDIFFZ to exercise the real encoder and hpatchz")
	}
	if _, err := exec.LookPath("hpatchz"); err != nil {
		t.Fatal(err)
	}
	d := t.TempDir()
	old, next, patch, output := filepath.Join(d, "old"), filepath.Join(d, "next"), filepath.Join(d, "patch"), filepath.Join(d, "output")
	before := []byte("original battery payload\n")
	after := []byte("new battery payload with extra bytes\n")
	if err := os.WriteFile(old, before, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(next, after, 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(hdiff, "-m-4", "-p-2", "-c-zstd-20", old, next, patch).CombinedOutput(); err != nil {
		t.Fatalf("encode: %v %s", err, out)
	}
	tracker := newProgressTracker(int64(len(after)), func(int, string) {})
	got, err := ApplyHDiff(context.Background(), old, patch, output, tracker)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(after)
	if got != hex.EncodeToString(sum[:]) {
		t.Fatal("output hash mismatch")
	}
	data, err := os.ReadFile(output)
	if err != nil || string(data) != string(after) {
		t.Fatalf("output %q, %v", data, err)
	}
}

func TestApplyHDiffFailureAndCancellation(t *testing.T) {
	d := t.TempDir()
	bin := filepath.Join(d, "hpatchz")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf partial > \"$4\"\nexit 2\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", d)
	out := filepath.Join(d, "out")
	tracker := newProgressTracker(100, func(int, string) {})
	if _, err := ApplyHDiff(context.Background(), "old", "patch", out, tracker); err == nil {
		t.Fatal("failure accepted")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("partial output retained")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ApplyHDiff(ctx, "old", "patch", out, tracker); err == nil {
		t.Fatal("cancellation accepted")
	}
}
