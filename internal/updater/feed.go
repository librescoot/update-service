package updater

import (
	"os"
	"os/exec"
	"strings"

	"github.com/librescoot/update-service/internal/mender"
)

const defaultReleaseFeed = "https://downloads.librescoot.org/releases"
const v2ImageMarker = "/usr/share/librescoot/ota-feed-v2"

// releaseFeedBase upgrades only the official feed and only after this image's
// capabilities and committed Mender identity agree. A staged intermediate
// artifact, Redis status or a separately upgraded binary is not a bridge boot.
func (u *Updater) releaseFeedBase(base string) string {
	if strings.TrimRight(base, "/") != defaultReleaseFeed {
		return base
	}
	marker, err := u.readV2Marker()
	if err != nil {
		return base
	}
	lines := strings.Split(string(marker), "\n")
	if len(lines) != 3 || lines[0] != "v2" || lines[1] == "" || lines[2] != "" {
		return base
	}
	if err := u.checkV2Decoder(); err != nil {
		return base
	}
	obs, err := u.observeUpdate()
	if err != nil || obs.State != mender.StateNoUpdate || obs.PendingArtifact != "" || obs.CommittedArtifact != lines[1] || obs.CommittedVersion == "" {
		return base
	}
	running, err := u.runningVersion()
	if err != nil || running == "" || !sameObservedVersion(running, obs.CommittedVersion) {
		return base
	}
	if u.hasUnresolvedActivation() {
		return base
	}
	return defaultReleaseFeed + "/v2"
}

func readV2Marker() ([]byte, error) { return os.ReadFile(v2ImageMarker) }
func checkV2Decoder() error {
	_, err := exec.LookPath("zstd")
	return err
}
