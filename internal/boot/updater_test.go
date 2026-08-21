package boot

import (
	"os"
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
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}

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

// detectBootDeviceFromFile is a test helper for DetectBootDevice.
func detectBootDeviceFromFile(mountsPath, mountPoint string) (string, error) {
	f, err := os.Open(mountsPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return detectFromReader(f, mountPoint)
}
