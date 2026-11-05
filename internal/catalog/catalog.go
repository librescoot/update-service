// Package catalog describes explicit, board-specific OTA artifact connections.
package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
)

const MaxHops = 3

type Object struct {
	URL    string `json:"url"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type Artifact struct {
	ID             string   `json:"id"`
	Variant        string   `json:"variant_id"`
	Version        string   `json:"version"`
	Name           string   `json:"artifact_name"`
	Devices        []string `json:"device_types"`
	ManifestSHA256 string   `json:"manifest_sha256"`
	HeaderSHA256   string   `json:"header_sha256"`
	RootfsSHA256   string   `json:"rootfs_sha256"`
	PayloadSHA256  string   `json:"payload_tar_sha256"`
	PayloadSize    int64    `json:"payload_tar_size"`
	Full           Object   `json:"full"`
}

type Edge struct {
	Source string `json:"source"`
	Target string `json:"target"`
	URL    string `json:"url"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type Catalog struct {
	Schema    int                          `json:"schema_version"`
	Revision  string                       `json:"revision"`
	Targets   map[string]map[string]string `json:"targets"`
	Artifacts []Artifact                   `json:"artifacts"`
	Deltas    []Edge                       `json:"deltas"`
}

func Hash(data []byte) string { s := sha256.Sum256(data); return hex.EncodeToString(s[:]) }
func (a Artifact) Identity() string {
	return Hash([]byte(a.Variant + "\n" + a.Name + "\n" + a.ManifestSHA256 + "\n" + a.HeaderSHA256 + "\n"))
}
func ValidDigest(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32 && s == strings.ToLower(s)
}
func (o Object) Validate(ext string) error {
	u, err := url.Parse(o.URL)
	if err != nil || u.Host == "" || u.Scheme != "https" || u.RawQuery != "" || u.Fragment != "" || u.User != nil || !strings.HasSuffix(u.Path, ext) || path.Base(u.Path) == "." || o.Size <= 0 || o.Size > 8<<30 || !ValidDigest(o.SHA256) {
		return errors.New("invalid OTA transfer object")
	}
	return nil
}
func (c *Catalog) Find(id string) (Artifact, bool) {
	for _, a := range c.Artifacts {
		if a.ID == id {
			return a, true
		}
	}
	return Artifact{}, false
}
func (c *Catalog) Target(channel, variant string) (Artifact, bool) {
	return c.Find(c.Targets[channel][variant])
}
func (c *Catalog) Validate() error {
	if c.Schema != 2 || len(c.Artifacts) > 512 || len(c.Deltas) > 2048 || !ValidDigest(c.Revision) {
		return errors.New("invalid OTA catalog version/limits/revision")
	}
	seen := map[string]bool{}
	names := map[string]bool{}
	for _, a := range c.Artifacts {
		if !ValidDigest(a.ID) || a.Identity() != a.ID || !ValidDigest(a.RootfsSHA256) || !ValidDigest(a.PayloadSHA256) || !ValidDigest(a.ManifestSHA256) || !ValidDigest(a.HeaderSHA256) || a.PayloadSize <= 0 || a.PayloadSize > 8<<30 || a.Version == "" || a.Name != "release-"+a.Version || seen[a.ID] {
			return errors.New("invalid or duplicate artifact identity")
		}
		if a.Variant != "unu-mdb" && a.Variant != "unu-dbc" {
			return errors.New("unsupported artifact variant")
		}
		compatible := false
		for _, d := range a.Devices {
			compatible = compatible || d == a.Variant
		}
		if !compatible {
			return errors.New("artifact has no compatible device type")
		}
		key := a.Variant + "/" + strings.ToLower(a.Version)
		if names[key] {
			return errors.New("ambiguous artifact version")
		}
		names[key] = true
		seen[a.ID] = true
		if err := a.Full.Validate(".mender"); err != nil {
			return err
		}
		if path.Base(a.Full.URL) != "librescoot-"+a.Variant+"-"+a.Version+".mender" {
			return errors.New("noncanonical full artifact filename")
		}
	}
	edges := map[string]bool{}
	outgoing := map[string]int{}
	for _, e := range c.Deltas {
		a, ok := c.Find(e.Source)
		b, bok := c.Find(e.Target)
		if !ok || !bok || e.Source == e.Target || a.Variant != b.Variant || edges[e.Source+e.Target] {
			return errors.New("invalid or duplicate delta endpoints")
		}
		edges[e.Source+e.Target] = true
		outgoing[e.Source]++
		if outgoing[e.Source] > 16 {
			return errors.New("too many delta edges from one artifact")
		}
		if err := (Object{e.URL, e.Size, e.SHA256}).Validate(".delta"); err != nil {
			return err
		}
		want := "librescoot-" + a.Variant + "-from-" + e.Source + "-to-" + e.Target + ".delta"
		u, _ := url.Parse(e.URL)
		if path.Base(u.Path) != want {
			return errors.New("noncanonical delta filename")
		}
	}
	for ch, variants := range c.Targets {
		if ch != "stable" && ch != "testing" {
			return fmt.Errorf("unsupported catalog channel %s", ch)
		}
		for v, id := range variants {
			a, ok := c.Find(id)
			if !ok || a.Variant != v {
				return errors.New("invalid channel target")
			}
		}
	}
	return nil
}

type Plan struct {
	Schema   int      `json:"schema_version"`
	Revision string   `json:"revision"`
	Channel  string   `json:"channel"`
	Base     Artifact `json:"base"`
	Target   Artifact `json:"target"`
	Steps    []Edge   `json:"steps"`
	// Nodes retain the exact metadata for intermediate identity checks after restart.
	Nodes          []Artifact `json:"nodes"`
	InstallStarted bool       `json:"install_started,omitempty"`
}

func (p Plan) Method() string {
	if len(p.Steps) > 0 {
		return "delta"
	}
	return "full"
}
func (p Plan) Bytes() int64 {
	if len(p.Steps) == 0 {
		return p.Target.Full.Size
	}
	var n int64
	for _, e := range p.Steps {
		n += e.Size
	}
	return n
}

// Route optimizes transfers to one already-selected target. It never chooses a
// version or a channel and never treats a version string as a patch base.
func (c *Catalog) Route(base, target Artifact, channel string, delta bool) Plan {
	p := Plan{Schema: 2, Revision: c.Revision, Channel: channel, Base: base, Target: target}
	if !delta || base.ID == "" || base.ID == target.ID {
		return p
	}
	best := target.Full.Size
	var walk func(string, []Edge, map[string]bool, int64)
	walk = func(id string, steps []Edge, seen map[string]bool, cost int64) {
		if id == target.ID {
			if cost < best || (cost == best && len(p.Steps) > 0 && len(steps) < len(p.Steps)) {
				best = cost
				p.Steps = append([]Edge(nil), steps...)
			}
			return
		}
		if len(steps) == MaxHops {
			return
		}
		for _, e := range c.Deltas {
			if e.Source == id && !seen[e.Target] && e.Size < best-cost {
				seen[e.Target] = true
				walk(e.Target, append(steps, e), seen, cost+e.Size)
				delete(seen, e.Target)
			}
		}
	}
	walk(base.ID, nil, map[string]bool{base.ID: true}, 0)
	if len(p.Steps) > 0 {
		p.Nodes = append(p.Nodes, base)
		for _, e := range p.Steps {
			a, _ := c.Find(e.Target)
			p.Nodes = append(p.Nodes, a)
		}
	}
	return p
}
func (p Plan) Validate() error {
	nodes := p.Nodes
	if len(p.Steps) == 0 {
		nodes = []Artifact{p.Target}
	}
	c := Catalog{Schema: p.Schema, Revision: p.Revision, Targets: map[string]map[string]string{p.Channel: {p.Target.Variant: p.Target.ID}}, Artifacts: nodes, Deltas: p.Steps}
	if err := c.Validate(); err != nil {
		return err
	}
	if len(p.Steps) > MaxHops {
		return errors.New("too many delta steps")
	}
	id := p.Base.ID
	for _, e := range p.Steps {
		if e.Source != id {
			return errors.New("disconnected delta plan")
		}
		id = e.Target
	}
	if len(p.Steps) > 0 && (id != p.Target.ID || p.Nodes[0].ID != p.Base.ID || p.Nodes[len(p.Nodes)-1].ID != p.Target.ID) {
		return errors.New("delta plan has wrong target/base")
	}
	return nil
}
