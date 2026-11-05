package updater

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/librescoot/update-service/internal/catalog"
	"github.com/librescoot/update-service/internal/config"
	"github.com/librescoot/update-service/internal/mender/delta"
)

// getCatalogV2 is deliberately bounded: absence of the optional catalog must
// not delay or disable legacy clients and releases.
func (g *GitHubAPI) getCatalogV2(ctx context.Context) (*catalog.Catalog, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.baseURL+"/ota-v2.json", nil)
	if err != nil {
		return nil, err
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OTA catalog HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 4<<20 {
		return nil, fmt.Errorf("OTA catalog too large")
	}
	// The revision binds the entire catalog except the revision field itself.
	var raw map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err = decoder.Decode(&raw); err != nil {
		return nil, err
	}
	revision, _ := raw["revision"].(string)
	delete(raw, "revision")
	var canonical bytes.Buffer
	encoder := json.NewEncoder(&canonical)
	encoder.SetEscapeHTML(false)
	if err = encoder.Encode(raw); err != nil {
		return nil, err
	}
	if catalog.Hash(bytes.TrimSuffix(canonical.Bytes(), []byte("\n"))) != revision {
		return nil, fmt.Errorf("OTA catalog revision mismatch")
	}
	var c catalog.Catalog
	if err = json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	if err = c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// candidateV2 resolves the already selected legacy release to one exact v2
// target. A stale or missing v2 index must never redirect an update to an old image.
func (u *Updater) candidateV2(ctx context.Context, channel, variant, current string, release Release, method string) (catalog.Plan, bool) {
	if u.mender == nil || current == "" || (channel != "stable" && channel != "testing") {
		return catalog.Plan{}, false
	}
	c, err := u.githubAPI.getCatalogV2(ctx)
	if err != nil {
		u.logger.Printf("OTA v2 unavailable: %v", err)
		return catalog.Plan{}, false
	}
	target, ok := c.Target(channel, variant)
	if !ok || !strings.EqualFold(target.Version, release.TagName) {
		return catalog.Plan{}, false
	}
	a, ok := releaseAsset(release, variant, ".mender")
	if !ok || a.URL != target.Full.URL || a.Size != target.Full.Size || a.Sha256 == "" || a.Sha256 != target.Full.SHA256 {
		return catalog.Plan{}, false
	}
	p := catalog.Plan{Schema: 2, Revision: c.Revision, Channel: channel, Target: target}
	if method != "delta" {
		return p, true
	}
	basePath, found := u.mender.FindMenderFileForVersion(current)
	if !found {
		return p, true
	}
	meta, err := catalog.Inspect(ctx, basePath, variant, false)
	if err != nil {
		return p, true
	}
	base, found := c.Find(meta.ID)
	if !found || !strings.EqualFold(base.Version, current) {
		return p, true
	}
	// The lightweight metadata identification is enough for planning. The whole
	// base is verified before any patch is applied.
	return c.Route(base, target, channel, true), true
}

func planFile(dir string) string { return filepath.Join(dir, "plan-v2.json") }
func saveV2Plan(dir string, p catalog.Plan) error {
	if err := p.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".plan-v2-*")
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
	if err = os.Rename(tmp, planFile(dir)); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func loadV2Plan(dir string) (catalog.Plan, error) {
	var p catalog.Plan
	data, err := os.ReadFile(planFile(dir))
	if err != nil {
		return p, err
	}
	if len(data) > 1<<20 {
		return p, fmt.Errorf("plan too large")
	}
	if err = json.Unmarshal(data, &p); err != nil {
		return p, err
	}
	return p, p.Validate()
}
func clearV2Plan(dir string) error {
	err := os.Remove(planFile(dir))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func enoughDeltaSpace(p catalog.Plan, dir string) bool {
	var s syscall.Statfs_t
	if err := syscall.Statfs(dir, &s); err != nil {
		return false
	}
	// Two expanded payloads, extracted compressed base, target and downloads.
	// Scratch and destination normally share /data; reject if they do not.
	if err := os.MkdirAll("/data/ota/tmp", 0755); err != nil {
		return false
	}
	var scratch syscall.Statfs_t
	if err := syscall.Statfs("/data/ota/tmp", &scratch); err != nil || scratch.Fsid != s.Fsid {
		return false
	}
	maxPayload := p.Base.PayloadSize
	for _, a := range p.Nodes {
		if a.PayloadSize > maxPayload {
			maxPayload = a.PayloadSize
		}
	}
	need := uint64(2*maxPayload + p.Base.Full.Size + p.Target.Full.Size + p.Bytes() + 64*1024*1024)
	return uint64(s.Bavail)*uint64(s.Bsize) >= need
}

// verifyV2Deltas binds the downloaded edge metadata to the plan before the
// legacy applier is allowed to unpack or reconstruct anything.
func verifyV2Deltas(p catalog.Plan, paths []string) error {
	if len(paths) != len(p.Steps) || len(p.Nodes) != len(paths)+1 {
		return fmt.Errorf("wrong delta chain length")
	}
	for i, file := range paths {
		meta, err := delta.ReadMetadata(file)
		if err != nil {
			return err
		}
		old, next := p.Nodes[i], p.Nodes[i+1]
		if meta.OldArtifactName != old.Name || meta.NewArtifactName != next.Name || meta.OldPayloadChecksum != old.RootfsSHA256 || meta.NewPayloadChecksum != next.RootfsSHA256 {
			return fmt.Errorf("delta %d metadata differs from plan", i+1)
		}
		modified := 0
		for _, change := range meta.Changes {
			if change.Type == "modified" {
				modified++
				if change.OldMeta.DecompressedSHA256 != old.PayloadSHA256 || change.NewMeta.DecompressedSHA256 != next.PayloadSHA256 {
					return fmt.Errorf("delta %d payload digest differs from plan", i+1)
				}
			}
		}
		if modified != 1 {
			return fmt.Errorf("delta %d does not contain one rootfs patch", i+1)
		}
	}
	return nil
}

func v2PlanTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 5*time.Second)
}
func isCrossChannel(current, channel string) bool {
	v := config.InferChannelFromVersion(current)
	return v != "" && v != channel
}
