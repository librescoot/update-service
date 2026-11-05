package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/librescoot/update-service/internal/config"
)

func TestOfflineEnrollment(t *testing.T) {
	if os.Getenv("OTA_ENROLL_HELPER") == "1" {
		os.Args = []string{"update-service", "--component=mdb", "--enroll-feed=canary", "--channel=testing", "--feed-config=" + os.Getenv("OTA_ENROLL_PATH")}
		main()
		return
	}
	path := filepath.Join(t.TempDir(), "feed.json")
	cmd := exec.Command(os.Args[0], "-test.run=^TestOfflineEnrollment$")
	cmd.Env = append(os.Environ(), "OTA_ENROLL_HELPER=1", "OTA_ENROLL_PATH="+path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("offline enrollment: %v\n%s", err, out)
	}
	p, err := config.LoadFeedPolicy(path)
	if err != nil || p.URL != config.CanaryV2Feed || p.Channel != "testing" {
		t.Fatalf("wrong enrollment: %+v %v", p, err)
	}
}
