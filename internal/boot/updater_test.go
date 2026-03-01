package boot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectBootDevice(t *testing.T) {
	tests := []struct {
		name       string
		mounts     string
		mountPoint string
		want       string
		wantErr    bool
	}{
		{
			name: "mmcblk3p1 at /uboot",
			mounts: `sysfs /sys sysfs rw 0 0
proc /proc proc rw 0 0
/dev/mmcblk3p2 / ext4 rw 0 0
/dev/mmcblk3p1 /uboot vfat rw 0 0
tmpfs /tmp tmpfs rw 0 0
`,
			mountPoint: "/uboot",
			want:       "/dev/mmcblk3boot0",
		},
		{
			name: "mmcblk1p1 at /uboot",
			mounts: `/dev/mmcblk1p1 /uboot vfat ro 0 0
/dev/mmcblk1p2 / ext4 rw 0 0
`,
			mountPoint: "/uboot",
			want:       "/dev/mmcblk1boot0",
		},
		{
			name: "mount point not found",
			mounts: `/dev/mmcblk3p2 / ext4 rw 0 0
`,
			mountPoint: "/uboot",
			wantErr:    true,
		},
		{
			name: "no partition suffix",
			mounts: `/dev/sda /uboot vfat rw 0 0
`,
			mountPoint: "/uboot",
			// /dev/sda has no 'p' partition suffix so base stays as /dev/sda
			want: "/dev/sdaboot0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Write fake /proc/mounts to a temp file
			f, err := os.CreateTemp(t.TempDir(), "mounts")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString(tt.mounts); err != nil {
				t.Fatal(err)
			}
			f.Close()

			// Patch the function to use our temp file by monkey-patching Open
			// Since we can't easily mock os.Open, we use an internal helper instead.
			// For the test, we use detectBootDeviceFromFile.
			got, err := detectBootDeviceFromFile(f.Name(), tt.mountPoint)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWriteFileVerified(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	dst := filepath.Join(dir, "dst.bin")

	data := []byte("hello boot world")
	if err := os.WriteFile(src, data, 0644); err != nil {
		t.Fatal(err)
	}

	if err := writeFileVerified(dst, src); err != nil {
		t.Fatalf("writeFileVerified failed: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Errorf("dst content mismatch: got %q, want %q", got, data)
	}
}

func TestGetInstalledVersion(t *testing.T) {
	dir := t.TempDir()
	b := &BootUpdater{
		versionFile: filepath.Join(dir, "boot-version"),
	}

	// Missing file → ""
	ver, err := b.GetInstalledVersion()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ver != "" {
		t.Errorf("expected empty version, got %q", ver)
	}

	// Write a version
	if err := os.WriteFile(b.versionFile, []byte("nightly-20260301T013104\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ver, err = b.GetInstalledVersion()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ver != "nightly-20260301T013104" {
		t.Errorf("unexpected version: %q", ver)
	}
}

func TestWriteVersionFile(t *testing.T) {
	dir := t.TempDir()
	b := &BootUpdater{
		mountPoint:  dir,
		versionFile: filepath.Join(dir, "boot-version"),
	}

	if err := b.WriteVersionFile("nightly-20260301T013104"); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(b.versionFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "nightly-20260301T013104") {
		t.Errorf("version file content unexpected: %q", string(data))
	}
}

// detectBootDeviceFromFile is a test helper for DetectBootDevice.
func detectBootDeviceFromFile(mountsPath, mountPoint string) (string, error) {
	f, err := os.Open(mountsPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return detectFromReader(f, mountPoint)
}
