package catalog

import (
	"fmt"
	"strings"
	"testing"
)

func artifact(v string) Artifact {
	a := Artifact{Variant: "unu-mdb", Version: v, Name: "release-" + v, Devices: []string{"unu-mdb"}, ManifestSHA256: Hash([]byte(v)), HeaderSHA256: Hash([]byte("header" + v)), RootfsSHA256: Hash([]byte("root" + v)), PayloadSHA256: Hash([]byte("tar" + v)), PayloadSize: 2048, Full: Object{URL: "https://example.org/librescoot-unu-mdb-" + v + ".mender", Size: 1000, SHA256: Hash([]byte("full" + v))}}
	a.ID = a.Identity()
	return a
}
func edge(a, b Artifact, n int64) Edge {
	return Edge{Source: a.ID, Target: b.ID, URL: fmt.Sprintf("https://example.org/librescoot-unu-mdb-from-%s-to-%s.delta", a.ID, b.ID), Size: n, SHA256: Hash([]byte(a.ID + b.ID))}
}
func TestRoutes(t *testing.T) {
	a, b, c := artifact("v1.4.1"), artifact("testing-20261105T120000"), artifact("v1.4.2")
	cat := Catalog{Schema: 2, Revision: Hash([]byte("rev")), Targets: map[string]map[string]string{"stable": {"unu-mdb": c.ID}}, Artifacts: []Artifact{a, b, c}, Deltas: []Edge{edge(a, c, 400), edge(a, b, 100), edge(b, c, 100), edge(b, a, 50)}}
	if err := cat.Validate(); err != nil {
		t.Fatal(err)
	}
	p := cat.Route(a, c, "stable", true)
	if p.Bytes() != 200 || len(p.Steps) != 2 {
		t.Fatalf("wrong route: %+v", p)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	reverse := cat.Route(b, a, "stable", true)
	if reverse.Bytes() != 50 {
		t.Fatal("reverse edge not used")
	}
	full := cat.Route(a, c, "stable", false)
	if full.Method() != "full" {
		t.Fatal("full setting ignored")
	}
	cat.Deltas = nil
	if cat.Route(a, c, "stable", true).Method() != "full" {
		t.Fatal("missing route guessed")
	}
}
func TestInvalidCatalog(t *testing.T) {
	a, b := artifact("v1.4.1"), artifact("testing-20261105T120000")
	for _, mutate := range []func(*Catalog){
		func(c *Catalog) { c.Schema = 3 }, func(c *Catalog) { c.Artifacts[0].ID = strings.Repeat("0", 64) },
		func(c *Catalog) { c.Artifacts[1].Variant = "unu-dbc" }, func(c *Catalog) { c.Deltas[0].SHA256 = "" },
		func(c *Catalog) { c.Deltas[0].Target = a.ID }, func(c *Catalog) { c.Deltas = append(c.Deltas, c.Deltas[0]) },
	} {
		cat := Catalog{Schema: 2, Revision: Hash([]byte("rev")), Artifacts: []Artifact{a, b}, Deltas: []Edge{edge(a, b, 100)}}
		mutate(&cat)
		if cat.Validate() == nil {
			t.Fatal("invalid catalog accepted")
		}
	}
}
func TestHopLimit(t *testing.T) {
	var cat Catalog
	var nodes []Artifact
	for i := 0; i < 5; i++ {
		nodes = append(nodes, artifact(fmt.Sprintf("v1.0.%d", i)))
		if i > 0 {
			cat.Deltas = append(cat.Deltas, edge(nodes[i-1], nodes[i], 1))
		}
	}
	cat.Artifacts = nodes
	if cat.Route(nodes[0], nodes[4], "stable", true).Method() != "full" {
		t.Fatal("four-hop route accepted")
	}
}
