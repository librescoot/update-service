package boot

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const testMounts = `sysfs /sys sysfs rw 0 0
proc /proc proc rw 0 0
/dev/mmcblk3p2 / ext4 rw 0 0
/dev/mmcblk3p1 /uboot vfat ro 0 0
/dev/mmcblk3p4 /data ext4 rw 0 0
tmpfs /tmp tmpfs rw 0 0
`

func TestBootTargets(t *testing.T) {
	tests := []struct {
		name      string
		component string
		want      []string
		wantErr   bool
	}{
		{
			name:      "dbc boots the user area",
			component: "dbc",
			want:      []string{"/dev/mmcblk3"},
		},
		{
			name:      "mdb boots the user area",
			component: "mdb",
			want:      []string{"/dev/mmcblk3"},
		},
		{
			name:      "unknown component is refused",
			component: "rpi4",
			wantErr:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := bootTargetsFromReader(strings.NewReader(testMounts), tt.component, "/uboot")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("accepted component %q, got %v", tt.component, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
	if _, err := bootTargetsFromReader(strings.NewReader(testMounts), "dbc", "/boot"); err == nil {
		t.Fatal("accepted a mount point that is not mounted")
	}
}

// Every region is written, and only a boot partition goes through force_ro.
func TestTargetsApplyEveryRegion(t *testing.T) {
	data := representativeIMX(false)
	const firstPartition = 8192 * 512

	userArea, dir, userState := testUserAreaUpdater(t, data)
	userState.partStart = firstPartition
	bootPart, _, bootState := testUpdater(t, data)

	targets := &Targets{
		updaters: []*BootUpdater{userArea, bootPart},
		logger:   log.New(io.Discard, "", 0),
	}
	if same, err := targets.UpToDate(dir); err != nil || same {
		t.Fatalf("UpToDate before apply = %v, %v; want false", same, err)
	}
	if err := targets.Apply(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if userState.writes != 1 || len(userState.locks) != 0 {
		t.Fatalf("user area: writes=%d locks=%v", userState.writes, userState.locks)
	}
	if bootState.writes != 1 || len(bootState.locks) != 2 || bootState.locks[0] || !bootState.locks[1] {
		t.Fatalf("boot partition: writes=%d locks=%v", bootState.writes, bootState.locks)
	}
	if same, err := targets.UpToDate(dir); err != nil || !same {
		t.Fatalf("UpToDate after apply = %v, %v; want true", same, err)
	}
}

func TestBootTargetsPrimaryIsTheFlashedRegion(t *testing.T) {
	// The first target is the region the flash path writes, on both machines.
	for _, component := range []string{"mdb", "dbc"} {
		got, err := bootTargetsFromReader(strings.NewReader(testMounts), component, "/uboot")
		if err != nil || got[0] != "/dev/mmcblk3" {
			t.Fatalf("%s primary target = %v, %v", component, got, err)
		}
	}
}

func TestKindOfDevicePath(t *testing.T) {
	tests := map[string]TargetKind{
		"/dev/mmcblk3boot0": regionBootPartition,
		"/dev/mmcblk1boot0": regionBootPartition,
		"/dev/mmcblk3":      regionUserArea,
		"/dev/mmcblk0":      regionUserArea,
	}
	for path, want := range tests {
		got, ok := kindOfDevicePath(path)
		if !ok || got != want {
			t.Errorf("kindOfDevicePath(%q) = %v, %v; want %v", path, got, ok, want)
		}
	}
	for _, path := range []string{
		"/dev/mmcblk3p1",    // a partition, not a boot region
		"/dev/mmcblk3boot1", // boot partition 2: nothing boots from it
		"/dev/mmcblk3boot",  // neither shape
		"/dev/sda",
		"/tmp/target",
		"/dev/../dev/mmcblk3boot0",
		"",
	} {
		if kind, ok := kindOfDevicePath(path); ok {
			t.Errorf("kindOfDevicePath(%q) = %v, want rejected", path, kind)
		}
	}
}

// A user-area target shares the device with the partitions, so the only safe
// place for the image is the gap ahead of the first one.
func TestUserAreaWrite(t *testing.T) {
	// The DBC's first partition starts at sector 8192, well past the image.
	const firstPartition = 8192 * 512
	data := representativeIMX(false)

	t.Run("writes ahead of the first partition", func(t *testing.T) {
		b, dir, s := testUserAreaUpdater(t, data)
		s.partStart = firstPartition
		if err := b.Apply(context.Background(), dir); err != nil {
			t.Fatal(err)
		}
		if s.writes != 1 || s.syncs != 1 {
			t.Fatalf("unexpected I/O: %+v", s)
		}
		// The user area has no force_ro switch to open.
		if len(s.locks) != 0 {
			t.Fatalf("touched force_ro: %v", s.locks)
		}
		got, err := os.ReadFile(filepath.Join(dir, "target"))
		if err != nil {
			t.Fatal(err)
		}
		if !equalAt(got, data, 1024) {
			t.Fatal("wrote to the wrong offset")
		}
		if same, err := b.UpToDate(dir); err != nil || !same {
			t.Fatalf("UpToDate = %v, %v", same, err)
		}
	})

	t.Run("refuses to reach into the first partition", func(t *testing.T) {
		b, dir, s := testUserAreaUpdater(t, data)
		s.partStart = 1024 + uint64(len(data)) - 1
		err := b.Apply(context.Background(), dir)
		if err == nil || !strings.Contains(err.Error(), "first partition") {
			t.Fatalf("got %v", err)
		}
		if s.writes != 0 || s.opens != 0 {
			t.Fatal("wrote to an unsafe target")
		}
	})

	t.Run("refuses when the layout cannot be read", func(t *testing.T) {
		b, dir, s := testUserAreaUpdater(t, data)
		s.partStartErr = io.ErrUnexpectedEOF
		if err := b.Apply(context.Background(), dir); err == nil {
			t.Fatal("accepted an unreadable layout")
		}
		if s.writes != 0 {
			t.Fatal("wrote without a layout")
		}
	})

	t.Run("refuses a partition path", func(t *testing.T) {
		b, dir, s := testUserAreaUpdater(t, data)
		b.bootDevice = "/dev/mmcblk3p1"
		if err := b.Apply(context.Background(), dir); err == nil {
			t.Fatal("accepted a partition as the target")
		}
		if s.writes != 0 {
			t.Fatal("wrote to a partition")
		}
	})
}

func equalAt(haystack, want []byte, off int) bool {
	if off < 0 || len(haystack) < off+len(want) {
		return false
	}
	for i := range want {
		if haystack[off+i] != want[i] {
			return false
		}
	}
	return true
}

func testUserAreaUpdater(t *testing.T, data []byte) (*BootUpdater, string, *ioState) {
	t.Helper()
	b, dir, state := testUpdater(t, data)
	b.bootDevice = "/dev/mmcblk3"
	// testUpdater logs and builds for boot0; the target file is the same.
	b.logger = log.New(io.Discard, "", 0)
	return b, dir, state
}
