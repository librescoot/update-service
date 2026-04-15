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
			want:       "/dev/mmcblk3",
		},
		{
			name: "mmcblk1p1 at /uboot",
			mounts: `/dev/mmcblk1p1 /uboot vfat ro 0 0
/dev/mmcblk1p2 / ext4 rw 0 0
`,
			mountPoint: "/uboot",
			want:       "/dev/mmcblk1",
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
			want:       "/dev/sda",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := os.CreateTemp(t.TempDir(), "mounts")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString(tt.mounts); err != nil {
				t.Fatal(err)
			}
			f.Close()

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

func TestStripBootSuffix(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"/dev/mmcblk3boot0", "/dev/mmcblk3"},
		{"/dev/mmcblk3boot1", "/dev/mmcblk3"},
		{"/dev/mmcblk1boot0", "/dev/mmcblk1"},
		{"/dev/mmcblk3", "/dev/mmcblk3"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := stripBootSuffix(tt.in); got != tt.want {
			t.Errorf("stripBootSuffix(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParsePartitionConfig(t *testing.T) {
	tests := []struct {
		name       string
		output     string
		wantActive int
		wantAck    int
		wantErr    bool
	}{
		{
			name:       "boot0 active with ack",
			output:     "Boot configuration bytes [PARTITION_CONFIG: 0x48]\n",
			wantActive: 1,
			wantAck:    1,
		},
		{
			name:       "boot1 active with ack",
			output:     "Boot configuration bytes [PARTITION_CONFIG: 0x50]\n",
			wantActive: 2,
			wantAck:    1,
		},
		{
			name:       "boot0 active no ack",
			output:     "Boot configuration bytes [PARTITION_CONFIG: 0x08]\n",
			wantActive: 1,
			wantAck:    0,
		},
		{
			name:       "no boot partition enabled",
			output:     "Boot configuration bytes [PARTITION_CONFIG: 0x00]\n",
			wantActive: 0,
			wantAck:    0,
		},
		{
			name:       "user area enabled",
			output:     "Boot configuration bytes [PARTITION_CONFIG: 0x38]\n",
			wantActive: 7,
			wantAck:    0,
		},
		{
			name:       "older label format",
			output:     "Boot Area Partition [PARTITION_CONFIG: 0x48]\n",
			wantActive: 1,
			wantAck:    1,
		},
		{
			name:       "embedded in larger extcsd dump",
			output:     "...\nBoot configuration: B_SIZE_MULT: 0x10\nBoot configuration bytes [PARTITION_CONFIG: 0x48]\nBoot bus Conditions [BOOT_BUS_CONDITIONS: 0x00]\n...",
			wantActive: 1,
			wantAck:    1,
		},
		{
			name:    "missing",
			output:  "Extended CSD rev 1.7 (MMC 5.0)\n",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			active, ack, err := parsePartitionConfig(tt.output)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got active=%d ack=%d", active, ack)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if active != tt.wantActive {
				t.Errorf("active = %d, want %d", active, tt.wantActive)
			}
			if ack != tt.wantAck {
				t.Errorf("ack = %d, want %d", ack, tt.wantAck)
			}
		})
	}
}

func TestInactivePartition(t *testing.T) {
	tests := []struct {
		active, want int
	}{
		{0, 1},
		{1, 2},
		{2, 1},
		{7, 1},
	}
	for _, tt := range tests {
		if got := inactivePartition(tt.active); got != tt.want {
			t.Errorf("inactivePartition(%d) = %d, want %d", tt.active, got, tt.want)
		}
	}
}

func TestBootPartitionDevice(t *testing.T) {
	tests := []struct {
		mmc  string
		n    int
		want string
	}{
		{"/dev/mmcblk3", 1, "/dev/mmcblk3boot0"},
		{"/dev/mmcblk3", 2, "/dev/mmcblk3boot1"},
		{"/dev/mmcblk1", 1, "/dev/mmcblk1boot0"},
		{"/dev/mmcblk1", 2, "/dev/mmcblk1boot1"},
	}
	for _, tt := range tests {
		if got := bootPartitionDevice(tt.mmc, tt.n); got != tt.want {
			t.Errorf("bootPartitionDevice(%q, %d) = %q, want %q", tt.mmc, tt.n, got, tt.want)
		}
	}
}

func TestForceROPathFor(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"/dev/mmcblk3boot0", "/sys/block/mmcblk3boot0/force_ro"},
		{"/dev/mmcblk3boot1", "/sys/block/mmcblk3boot1/force_ro"},
		{"/dev/mmcblk1boot1", "/sys/block/mmcblk1boot1/force_ro"},
	}
	for _, tt := range tests {
		if got := forceROPathFor(tt.in); got != tt.want {
			t.Errorf("forceROPathFor(%q) = %q, want %q", tt.in, got, tt.want)
		}
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

	ver, err := b.GetInstalledVersion()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ver != "" {
		t.Errorf("expected empty version, got %q", ver)
	}

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
