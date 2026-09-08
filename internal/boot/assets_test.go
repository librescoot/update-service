package boot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPackagedAssetIntegrity(t *testing.T) {
	for _, kind := range []string{"missing manifest", "empty manifest", "malformed", "bad hex", "duplicate", "conflicting duplicate", "mismatch", "wrong filename", "tail corruption", "tail truncation", "empty source", "missing source", "large source", "large manifest"} {
		t.Run(kind, func(t *testing.T) {
			data := representativeIMX(true)
			b, dir, state := testUpdater(t, data)
			path := filepath.Join(dir, UBootPath)
			manifestPath := filepath.Join(dir, manifestName)
			entry := sha256sum(data) + "  " + UBootPath + "\n"
			manifest := entry
			write := func(path string, data []byte) {
				t.Helper()
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "missing manifest":
				if err := os.Remove(manifestPath); err != nil {
					t.Fatal(err)
				}
			case "empty manifest":
				manifest = ""
			case "malformed":
				manifest = "not a checksum\n" + entry
			case "bad hex":
				manifest = strings.Repeat("z", 64) + "  " + UBootPath + "\n"
			case "duplicate":
				manifest = entry + entry
			case "conflicting duplicate":
				manifest = entry + strings.Repeat("0", 64) + " *" + UBootPath + "\n"
			case "mismatch":
				manifest = strings.Repeat("0", 64) + "  " + UBootPath + "\n"
			case "wrong filename":
				manifest = sha256sum(data) + "  ./" + UBootPath + "\n"
			case "tail corruption":
				data[len(data)-1] ^= 1
				write(path, data)
			case "tail truncation":
				write(path, data[:len(data)-1])
			case "empty source":
				write(path, nil)
			case "missing source":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "large source":
				if err := os.Truncate(path, maxIMXBytes+1); err != nil {
					t.Fatal(err)
				}
			case "large manifest":
				manifest = strings.Repeat("\n", maxManifestBytes+1)
			}
			if kind != "missing manifest" {
				write(manifestPath, []byte(manifest))
			}
			if _, err := b.UpToDate(dir); err == nil {
				t.Fatal("UpToDate accepted invalid package")
			}
			if err := b.Apply(context.Background(), dir); err == nil {
				t.Fatal("Apply accepted invalid package")
			}
			if state.opens != 0 || len(state.locks) != 0 || state.writes != 0 {
				t.Fatal("invalid package touched target")
			}
		})
	}
}

func TestMultiassetManifest(t *testing.T) {
	for _, marker := range []string{" ", "*"} {
		data := representativeIMX(true)
		b, dir, _ := testUpdater(t, data)
		manifest := sha256sum([]byte("kernel")) + "  zImage\n" + sha256sum([]byte("dtb")) + "  board.dtb\n" + strings.ToUpper(sha256sum(data)) + " " + marker + UBootPath + "\n"
		if err := os.WriteFile(filepath.Join(dir, manifestName), []byte(manifest), 0600); err != nil {
			t.Fatal(err)
		}
		if err := b.Apply(context.Background(), dir); err != nil {
			t.Fatal(err)
		}
		if same, err := b.UpToDate(dir); err != nil || !same {
			t.Fatalf("same=%v err=%v", same, err)
		}
	}
}
