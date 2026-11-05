package updater

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	ipc "github.com/librescoot/redis-ipc"
	"github.com/librescoot/update-service/internal/config"
	"github.com/librescoot/update-service/internal/mender"
)

// switchIntent is owned by the MDB, including while the DBC is powered off.
// Mender, not this journal, remains authoritative for each board's commit.
type switchIntent struct {
	ID             string `json:"id"`
	Channel        string `json:"channel"`
	MDBDone        bool   `json:"mdb_done"`
	DBCDone        bool   `json:"dbc_done"`
	MDBUnavailable bool   `json:"mdb_unavailable,omitempty"`
	DBCUnavailable bool   `json:"dbc_unavailable,omitempty"`
	MDBLastCheck   string `json:"mdb_last_check,omitempty"`
	DBCLastCheck   string `json:"dbc_last_check,omitempty"`
	Failed         bool   `json:"failed"`
}

func switchPath(dir string) string { return filepath.Join(dir, "channel-switch.json") }
func validSwitchID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, r := range id {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
func saveSwitch(dir string, s switchIntent) error {
	if !validSwitchID(s.ID) || !config.IsValidChannel(s.Channel) {
		return fmt.Errorf("invalid switch request")
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".channel-switch-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp, switchPath(dir)); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func readSwitch(dir string) (switchIntent, error) {
	var s switchIntent
	data, err := os.ReadFile(switchPath(dir))
	if err != nil {
		return s, err
	}
	if len(data) > 4096 {
		return s, fmt.Errorf("switch journal too large")
	}
	if err = json.Unmarshal(data, &s); err != nil {
		return s, err
	}
	if !validSwitchID(s.ID) || !config.IsValidChannel(s.Channel) {
		return s, fmt.Errorf("invalid switch journal")
	}
	return s, nil
}
func (u *Updater) publishSwitch(s switchIntent, state string) error {
	return u.redis.GetClient().NewHashPublisher("ota").SetMany(map[string]any{"switch-id": s.ID, "switch-channel": s.Channel, "switch-status": state, "switch-result": s.ID + ":" + state}, ipc.Sync())
}
func (u *Updater) publishCommittedVersion() {
	obs, err := u.observeUpdate()
	if err != nil || obs.PendingArtifact != "" || (obs.State != mender.StateNoUpdate && obs.State != mender.StateCommitted) {
		return
	}
	running, err := u.runningVersion()
	if err != nil || running == "" || !strings.EqualFold(running, obs.CommittedVersion) {
		return
	}
	u.publishCommittedArtifact(obs.CommittedArtifact)
}

func (u *Updater) publishCommittedArtifact(artifact string) {
	running := mender.VersionFromArtifact(artifact)
	if running == "" {
		return
	}
	if err := u.redis.GetClient().NewHashPublisher("ota").SetMany(map[string]any{"committed-version:" + u.config.Component: running}, ipc.Sync()); err != nil {
		u.logger.Printf("Publish committed version: %v", err)
	}
}
func (u *Updater) requestSwitch(channel, id string) {
	if u.config.Component != "mdb" || !validSwitchID(id) || !config.IsValidChannel(channel) {
		return
	}
	u.switchMu.Lock()
	defer u.switchMu.Unlock()
	s := switchIntent{ID: id, Channel: channel}
	if u.config.FeedURL != "" {
		if channel != "stable" && channel != "testing" {
			_ = u.publishSwitch(s, "error")
			return
		}
		for _, board := range []string{"mdb", "dbc"} {
			bound, err := u.redis.GetOTAField("ota", "feed-url:"+board)
			if err != nil || bound != u.config.FeedURL {
				_ = u.publishSwitch(s, "error")
				return
			}
		}
	}
	if !u.updateOpMu.TryLock() {
		_ = u.publishSwitch(s, "busy")
		return
	}
	defer u.updateOpMu.Unlock()
	if u.gateWindowIsOpen() {
		_ = u.publishSwitch(s, "busy")
		return
	}
	obs, err := u.observeUpdate()
	if err != nil || obs.PendingArtifact != "" || (obs.State != mender.StateNoUpdate && obs.State != mender.StateCommitted) {
		_ = u.publishSwitch(s, "busy")
		return
	}
	if _, err := os.Stat(planFile(u.mender.GetDownloadDir())); err == nil || !os.IsNotExist(err) {
		_ = u.publishSwitch(s, "busy")
		return
	}
	dir := u.mender.GetDownloadDir()
	if old, err := readSwitch(dir); err == nil {
		if old.ID == id {
			_ = u.publishSwitch(old, "accepted")
			return
		}
		if !old.Failed && !(old.MDBDone && old.DBCDone) {
			_ = u.publishSwitch(s, "busy")
			return
		}
	} else if !os.IsNotExist(err) {
		_ = u.publishSwitch(s, "error")
		return
	}
	for _, board := range []string{"mdb", "dbc"} {
		st, err := u.redis.GetOTAField("ota", "status:"+board)
		if err != nil && !strings.Contains(err.Error(), "nil") {
			_ = u.publishSwitch(s, "error")
			return
		}
		if st != "" && st != "idle" && st != "staged-noop" {
			_ = u.publishSwitch(s, "busy")
			return
		}
	}
	s.MDBLastCheck, _ = u.redis.GetClient().HGet("settings", "updates.mdb.last-check-time")
	s.DBCLastCheck, _ = u.redis.GetClient().HGet("settings", "updates.dbc.last-check-time")
	if err := saveSwitch(dir, s); err != nil {
		u.logger.Printf("Persist channel switch: %v", err)
		_ = u.publishSwitch(s, "error")
		return
	}
	u.switchQueuedMDB = false
	u.switchQueuedDBC = false
	u.switchPolicyLastQueued = time.Time{}
	if err := u.publishSwitch(s, "accepted"); err != nil {
		u.logger.Printf("Publish switch acceptance: %v", err)
	}
}
func (u *Updater) advanceSwitch() {
	u.switchMu.Lock()
	defer u.switchMu.Unlock()
	dir := u.mender.GetDownloadDir()
	s, err := readSwitch(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			u.logger.Printf("Read switch journal: %v", err)
		}
		return
	}
	if s.Failed {
		_ = u.publishSwitch(s, "failed")
		return
	}
	client := u.redis.GetClient()
	settings := client.NewHashPublisher("settings")
	for _, board := range []string{"mdb", "dbc"} {
		done := &s.MDBDone
		unavailable := &s.MDBUnavailable
		lastCheck := s.MDBLastCheck
		queued := &u.switchQueuedMDB
		if board == "dbc" {
			done = &s.DBCDone
			unavailable = &s.DBCUnavailable
			lastCheck = s.DBCLastCheck
			queued = &u.switchQueuedDBC
		}
		if *done {
			continue
		}
		if u.config.FeedURL != "" {
			result, _ := u.redis.GetOTAField("ota", "feed-channel-result:"+board)
			if result == s.ID+":error" {
				s.Failed = true
				break
			}
			if result != s.ID+":accepted" {
				if board == "mdb" {
					if err := u.config.SetFeedChannel(s.Channel); err != nil {
						s.Failed = true
						break
					}
					_ = client.NewHashPublisher("ota").SetMany(map[string]any{"feed-channel-result:mdb": s.ID + ":accepted"}, ipc.Sync())
				} else {
					// Keep only one queued request while off. Retry a live DBC
					// after a bounded interval if startup recovery was still busy.
					power, _ := client.HGet("vehicle", "dashboard:power")
					if !*queued || (power == "on" && time.Since(u.switchPolicyLastQueued) >= 2*time.Minute) {
						if err := u.redis.PushUpdateCommandToComponent(board, "set-feed-channel:"+s.Channel+":"+s.ID); err == nil {
							*queued = true
							u.switchPolicyLastQueued = time.Now()
						}
					}
					continue
				}
			}
		}
		state, _ := u.redis.GetOTAField("ota", "status:"+board)
		v, _ := u.redis.GetComponentVersion(board)
		committed, _ := u.redis.GetOTAField("ota", "committed-version:"+board)
		if state == "idle" && v != "" && strings.EqualFold(v, committed) && config.InferChannelFromVersion(v) == s.Channel {
			*done = true
			continue
		}
		checkTime, _ := client.HGet("settings", "updates."+board+".last-check-time")
		checkResult, _ := client.HGet("settings", "updates."+board+".last-attempt-result")
		if *queued && state == "idle" && checkTime != "" && checkTime != lastCheck && (checkResult == checkResultNoRelease || checkResult == checkResultNoAsset) {
			*done = true
			*unavailable = true
			continue
		}
		if state == "error" {
			s.Failed = true
			break
		}
		if err := settings.SetMany(map[string]any{"updates." + board + ".channel": s.Channel}, ipc.Sync()); err != nil {
			u.logger.Printf("Restore %s channel: %v", board, err)
			continue
		}
		if !*queued {
			if err := u.redis.PushUpdateCommandToComponent(board, "check-now"); err == nil {
				*queued = true
			} else {
				u.logger.Printf("Queue %s check: %v", board, err)
			}
		}
	}
	// The MDB preflight is what wakes an off DBC in stand-by. Even if the
	// MDB was already on the target channel it must get one check request.
	if s.MDBDone && !s.DBCDone && !u.switchQueuedMDB {
		if err := u.redis.PushUpdateCommandToComponent("mdb", "check-now"); err == nil {
			u.switchQueuedMDB = true
		}
	}
	if err := saveSwitch(dir, s); err != nil {
		u.logger.Printf("Save switch progress: %v", err)
		return
	}
	if s.Failed {
		_ = u.publishSwitch(s, "failed")
	} else if s.MDBDone && s.DBCDone {
		if s.MDBUnavailable || s.DBCUnavailable {
			_ = u.publishSwitch(s, "partial")
		} else {
			_ = u.publishSwitch(s, "complete")
		}
	} else {
		_ = u.publishSwitch(s, "accepted")
	}
}
func (u *Updater) monitorSwitch() {
	u.advanceSwitch()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-u.ctx.Done():
			return
		case <-ticker.C:
			u.advanceSwitch()
		}
	}
}
