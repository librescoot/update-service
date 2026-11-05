package updater

import (
	"context"
	"fmt"
	ipc "github.com/librescoot/redis-ipc"
	"os"
	"path/filepath"
	"strings"

	"github.com/librescoot/update-service/internal/catalog"
	"github.com/librescoot/update-service/internal/config"
	"github.com/librescoot/update-service/internal/mender"
	"github.com/librescoot/update-service/internal/status"
)

func (u *Updater) publishFeedBinding() error {
	return u.redis.GetClient().NewHashPublisher("ota").SetMany(map[string]any{
		"feed-url:" + u.config.Component:         u.config.FeedURL,
		"updater-protocol:" + u.config.Component: config.OTAProtocol,
	}, ipc.Sync())
}
func (u *Updater) setPersistentFeedChannel(channel, id string) {
	if u.config.FeedURL == "" || !validSwitchID(id) {
		return
	}
	if !u.updateOpMu.TryLock() {
		return
	}
	defer u.updateOpMu.Unlock()
	if _, err := os.Stat(planFile(u.mender.GetDownloadDir())); err == nil || !os.IsNotExist(err) {
		return
	}
	result := "error"
	if !u.gateWindowIsOpen() {
		obs, err := u.observeUpdate()
		if err == nil && obs.PendingArtifact == "" && (obs.State == mender.StateNoUpdate || obs.State == mender.StateCommitted) {
			if err := u.config.SetFeedChannel(channel); err == nil {
				result = "accepted"
			} else {
				u.logger.Printf("Persist feed channel: %v", err)
			}
		}
	}
	if err := u.redis.GetClient().NewHashPublisher("ota").SetMany(map[string]any{"feed-channel-result:" + u.config.Component: id + ":" + result}, ipc.Sync()); err != nil {
		u.logger.Printf("Feed channel result: %v", err)
	}
	if result == "accepted" {
		_ = u.redis.PushUpdateCommandToComponent(u.config.Component, "check-now")
	}
}

func releaseFromArtifact(a catalog.Artifact, channel string) Release {
	return Release{TagName: a.Version, Prerelease: channel != "stable", Assets: []Asset{{Name: filepath.Base(a.Full.URL), URL: a.Full.URL, Size: a.Full.Size, Sha256: a.Full.SHA256}}}
}
func (u *Updater) exclusiveTarget(c *catalog.Catalog, channel, variant string) (catalog.Artifact, error) {
	a, ok := c.Target(channel, variant)
	if !ok {
		return a, fmt.Errorf("no target for %s/%s", channel, variant)
	}
	if a.UpdaterProtocol != config.OTAProtocol {
		return a, fmt.Errorf("target %s does not preserve OTA protocol %d", a.Version, config.OTAProtocol)
	}
	r := releaseFromArtifact(a, channel)
	if !releaseMatchesChannel(r, channel) {
		return a, fmt.Errorf("target version does not match channel")
	}
	if len(u.withoutGateRejectedReleases([]Release{r})) == 0 {
		return a, fmt.Errorf("target was rejected by commit gate")
	}
	return a, nil
}
func (u *Updater) exclusivePlan(ctx context.Context, c *catalog.Catalog, a catalog.Artifact, channel, current, method string) catalog.Plan {
	p := catalog.Plan{Schema: 2, Revision: c.Revision, Channel: channel, Target: a, FeedURL: u.config.FeedURL}
	if method != "delta" || current == "" {
		return p
	}
	file, ok := u.mender.FindMenderFileForVersion(current)
	if !ok {
		return p
	}
	meta, err := catalog.Inspect(ctx, file, a.Variant, false)
	if err != nil {
		return p
	}
	base, ok := c.Find(meta.ID)
	if !ok || !strings.EqualFold(base.Version, current) {
		return p
	}
	p = c.Route(base, a, channel, true)
	p.FeedURL = u.config.FeedURL
	if p.Method() == "delta" && !enoughDeltaSpace(p, u.mender.GetDownloadDir()) {
		p.Steps = nil
		p.Nodes = nil
	}
	return p
}
func (u *Updater) checkExclusiveV2(current, channel, method string, manual bool) {
	c, err := u.githubAPI.getCatalogV2URL(u.ctx, u.config.FeedURL)
	if err != nil {
		u.logger.Printf("Exclusive feed unavailable; no legacy fallback: %v", err)
		u.failCheck(checkResultReleasesFailed)
		return
	}
	if u.config.Component == "mdb" {
		// Only the DBC release is needed for its preflight; each board selects its
		// actual transport independently after boot.
		variant, err := u.redis.GetVariantID("dbc")
		if err != nil || variant == "" {
			variant = "unu-dbc"
		}
		manifest := map[string]Release{}
		for _, ch := range []string{"stable", "testing"} {
			if a, e := u.exclusiveTarget(c, ch, variant); e == nil {
				manifest[ch] = releaseFromArtifact(a, ch)
			}
		}
		u.wg.Add(1)
		go func() { defer u.wg.Done(); u.orchestrateDBC(manifest) }()
	}
	variant, err := u.redis.GetVariantID(u.config.Component)
	if err != nil {
		u.failCheck(checkResultSettingsFailed)
		return
	}
	target, err := u.exclusiveTarget(c, channel, variant)
	if err != nil {
		u.logger.Printf("Exclusive feed target refused: %v", err)
		u.finishCheck(checkResultNoRelease)
		return
	}
	release := releaseFromArtifact(target, channel)
	if !u.isUpdateNeeded(release) {
		u.finishCheck(checkResultUpToDate)
		return
	}
	if skip, n := u.backoff.ShouldSkip(target.Version, u.config.CheckInterval); skip {
		_ = u.status.SetSkipChecksRemaining(u.ctx, n)
		u.finishCheck(checkResultBackedOff)
		return
	}
	p := u.exclusivePlan(u.ctx, c, target, channel, current, method)
	u.wg.Add(1)
	go func() { defer u.wg.Done(); u.executeV2(p, manual) }()
}

// Full fallback stays inside the same pinned feed and verifies the embedded
// updater capability before Mender can write a rootfs which would drop policy.
func (u *Updater) executeExclusiveFull(p catalog.Plan, manual bool) {
	p.Steps = nil
	p.Nodes = nil
	if err := saveV2Plan(u.mender.GetDownloadDir(), p); err != nil {
		u.logger.Printf("Persist exclusive full plan: %v", err)
		return
	}
	if err := u.status.SetDownloading(u.ctx, strings.ToLower(p.Target.Version), "full"); err != nil {
		return
	}
	u.finishCheck(checkResultUpdateStarted)
	if u.config.Component == "dbc" {
		if err := u.redis.PushUpdateCommand("start-dbc"); err != nil {
			u.logger.Printf("DBC start: %v", err)
		}
		defer u.completeDBCLifecycleIfSafe()
	}
	if u.config.Component == "mdb" {
		if err := u.inhibitor.AddDownloadInhibit(u.config.Component); err != nil {
			u.logger.Printf("Download inhibit: %v", err)
		}
		defer u.inhibitor.RemoveDownloadInhibit(u.config.Component)
	}
	defer u.holdSuspend()()
	var progress progressTracker
	file, err := u.mender.DownloadAndVerify(u.ctx, p.Target.Full.URL, "sha256:"+p.Target.Full.SHA256, func(done, total int64) { progress.observe(done); _ = u.status.SetDownloadProgress(u.ctx, done, total) })
	if err != nil {
		if u.ctx.Err() != nil {
			return
		}
		if isDownloadBudgetAbort(err) {
			u.recordDownloadAbort(p.Target.Version, err, progress.gained())
			return
		}
		_ = u.status.SetError(u.ctx, "download-failed", err.Error())
		return
	}
	if err := catalog.Verify(u.ctx, file, p.Target); err != nil {
		if u.ctx.Err() != nil {
			return
		}
		_ = u.status.SetError(u.ctx, "v2-target-invalid", err.Error())
		return
	}
	u.v2Install(p, file, manual)
}

func (u *Updater) previewExclusiveV2(ctx context.Context, channel string) {
	publish := func(state string) { _ = u.status.SetPreviewResult(u.ctx, channel, state, "", 0) }
	if channel != "stable" && channel != "testing" {
		publish(status.PreviewUnavailable)
		return
	}
	c, err := u.githubAPI.getCatalogV2URL(ctx, u.config.FeedURL)
	if err != nil {
		u.logger.Printf("Exclusive feed preview: %v", err)
		publish(status.PreviewError)
		return
	}
	variant, err := u.redis.GetVariantID(u.config.Component)
	if err != nil {
		publish(status.PreviewError)
		return
	}
	a, err := u.exclusiveTarget(c, channel, variant)
	if err != nil {
		publish(status.PreviewUnavailable)
		return
	}
	current, _ := u.getCurrentVersion()
	method := u.getUpdateMethod()
	p := u.exclusivePlan(ctx, c, a, channel, current, method)
	kind, bytes := p.Method(), p.Bytes()
	if !u.isUpdateNeeded(releaseFromArtifact(a, channel)) {
		kind = "none"
		bytes = 0
	}
	if err := u.status.SetPreviewResultWithPlan(u.ctx, channel, status.PreviewReady, a.Version, a.Full.Size, kind, bytes); err != nil {
		u.logger.Printf("Exclusive preview result: %v", err)
	}
}
