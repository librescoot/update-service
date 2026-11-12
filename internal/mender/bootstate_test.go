package mender

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseBootEnvironment(t *testing.T) {
	valid := "mender_boot_part=3\nmender_boot_part_hex=3\nupgrade_available=1\n"
	n, up, err := parseBootEnvironment(valid)
	if err != nil || n != 3 || !up {
		t.Fatalf("got %d %v %v", n, up, err)
	}
	for _, input := range []string{
		"mender_boot_part=2\nupgrade_available=0\n",
		"mender_boot_part=2\nmender_boot_part_hex=3\nupgrade_available=0\n",
		"mender_boot_part=0\nmender_boot_part_hex=0\nupgrade_available=0\n",
		"mender_boot_part=2\nmender_boot_part_hex=2\nupgrade_available=2\n",
		valid + "mender_boot_part=2\n",
		valid + "warning\n",
	} {
		if _, _, err := parseBootEnvironment(input); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
	n, up, err = parseBootEnvironment("upgrade_available=0\nmender_boot_part_hex=a\nmender_boot_part=10\n")
	if err != nil || n != 10 || up {
		t.Fatalf("decimal/hex parsed as %d %v %v", n, up, err)
	}
}

func TestParseOriginalPartition(t *testing.T) {
	n, err := parseOriginalPartition("orig_part_num=2\norig_part_num_hex=2\n")
	if err != nil || n != 2 {
		t.Fatalf("got %d %v", n, err)
	}
	for _, input := range []string{"orig_part_num=2", "orig_part_num=2\norig_part_num_hex=3", "orig_part_num=0\norig_part_num_hex=0", "orig_part_num=2\norig_part_num_hex=2\necho unsafe", "orig_part_num=2\norig_part_num=3\norig_part_num_hex=3", "orig_part_num=$(command)\norig_part_num_hex=2"} {
		if _, err := parseOriginalPartition(input); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
}

func TestReadRootfsOrigin(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "mmcblk1p2"), filepath.Join(dir, "mmcblk1p3")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	conf := filepath.Join(dir, "mender.conf")
	if err := os.WriteFile(conf, []byte(`{"RootfsPartA":"`+a+`","RootfsPartB":"`+b+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(dir, "tree")
	for _, p := range []string{"tmp", "header"} {
		if err := os.MkdirAll(filepath.Join(tree, p), 0700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name, data string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(tree, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("tmp/orig-part", "orig_part_num=2\norig_part_num_hex=2\n")
	write("header/artifact_name", "release-v1.4.0\n")
	write("current_artifact_name", "release-v1.3.1\n")
	o, err := readRootfsOrigin(tree, []string{conf})
	if err != nil || o.OriginalSlot != a || o.Artifact != "release-v1.4.0" || o.CommittedArtifact != "release-v1.3.1" {
		t.Fatalf("origin %+v %v", o, err)
	}
	write("tmp/orig-part", "orig_part_num=4\norig_part_num_hex=4\n")
	if _, err := readRootfsOrigin(tree, []string{conf}); err == nil {
		t.Fatal("accepted unknown original partition")
	}
	write("tmp/orig-part", "orig_part_num=2\norig_part_num_hex=2\n")
	write("current_artifact_name", "")
	if _, err := readRootfsOrigin(tree, []string{conf}); err == nil {
		t.Fatal("accepted absent committed identity")
	}
}

func TestDeviceNumberRejectsRegularFileAsSlot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slot")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := deviceNumber(path, true); err == nil {
		t.Fatal("regular file accepted as block device")
	}
	if _, err := deviceNumber("/", false); err != nil {
		t.Fatal(err)
	}
}
