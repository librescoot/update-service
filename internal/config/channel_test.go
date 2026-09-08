package config

import (
	"sync"
	"testing"
)

func TestChannelFallbackSurvivesRedisOverride(t *testing.T) {
	cfg := New("", "", 0, "dbc", "stable", "", false, false, "", "", 2)
	if !cfg.ApplyRedisUpdate("updates.dbc.channel", "testing") {
		t.Fatal("override rejected")
	}
	if cfg.FallbackChannel != "stable" || cfg.GetChannel() != "testing" {
		t.Fatal("Redis override replaced startup fallback")
	}
	if !cfg.ApplyRedisUpdate("updates.dbc.channel", "") || cfg.GetChannel() != "stable" {
		t.Fatal("removing override did not restore fallback")
	}
}

func TestConcurrentChannelReadAndNotification(t *testing.T) {
	cfg := &Config{Component: "dbc", Channel: "stable"}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			cfg.ApplyRedisUpdate("updates.dbc.channel", "testing")
			cfg.ApplyRedisUpdate("updates.dbc.channel", "nightly")
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			if !IsValidChannel(cfg.GetChannel()) {
				t.Error("invalid concurrent channel")
			}
		}
	}()
	wg.Wait()
}
