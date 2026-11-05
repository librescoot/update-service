package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPersistentFeedSurvivesRestartAndRedisOverrides(t *testing.T) {
	file := filepath.Join(t.TempDir(), "feed.json")
	p := FeedPolicy{Schema: 2, URL: CanaryV2Feed, Channel: "testing"}
	if err := SaveFeedPolicy(file, p); err != nil {
		t.Fatal(err)
	}
	c := &Config{Component: "mdb"}
	if err := c.BindFeed(file, p); err != nil {
		t.Fatal(err)
	}
	if c.ApplyRedisUpdate("updates.mdb.channel", "stable") || c.ApplyRedisUpdate("updates.mdb.releases-url", "https://example.org/releases") {
		t.Fatal("transient settings override enrollment")
	}
	if err := c.SetFeedChannel("stable"); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadFeedPolicy(file)
	if err != nil {
		t.Fatal(err)
	}
	next := &Config{}
	if err := next.BindFeed(file, loaded); err != nil {
		t.Fatal(err)
	}
	if ch, err := next.FeedChannel(); err != nil || ch != "stable" || next.FeedURL != CanaryV2Feed {
		t.Fatalf("lost enrollment: %s %v %+v", ch, err, next)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if _, err := next.FeedChannel(); err == nil {
		t.Fatal("removed policy silently accepted")
	}
}
func TestFeedPolicyValidation(t *testing.T) {
	for _, p := range []FeedPolicy{{Schema: 1, URL: CanaryV2Feed, Channel: "testing"}, {Schema: 2, URL: "http://example.org/canary.json", Channel: "testing"}, {Schema: 2, URL: CanaryV2Feed, Channel: "nightly"}} {
		if p.Validate() == nil {
			t.Fatalf("invalid policy accepted: %+v", p)
		}
	}
	file := filepath.Join(t.TempDir(), "feed.json")
	if err := os.WriteFile(file, []byte(`{"schema_version":2,"url":`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFeedPolicy(file); err == nil {
		t.Fatal("corrupt policy accepted")
	}
}
