package delta

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHDiffRealMenderChain(t *testing.T) {
	base, patch := os.Getenv("HDIFF_TEST_MENDER_BASE"), os.Getenv("HDIFF_TEST_DELTA")
	if base == "" && patch == "" {
		t.Skip("set HDIFF_TEST_MENDER_BASE and HDIFF_TEST_DELTA")
	}
	if base == "" || patch == "" {
		t.Fatal("both HDIFF_TEST_MENDER_BASE and HDIFF_TEST_DELTA are required")
	}
	d := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	err := NewApplier(log.New(io.Discard, "", 0), d).ApplyChain(ctx, base, []string{patch}, filepath.Join(d, "out.mender"), func(int, string) {})
	if err != nil {
		t.Fatal(err)
	}
}
