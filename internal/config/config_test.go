package config

import (
	"testing"
	"time"
)

func TestNew(t *testing.T) {
	// Test with default values
	cfg := New(
		"localhost:6379",
		"https://api.github.com/repos/librescoot/librescoot/releases",
		1*time.Hour,
		"stable",
		"dbc,mdb",
		"mender/update/dbc/url",
		"mender/update/mdb/url",
		"mender/update/dbc/checksum",
		"mender/update/mdb/checksum",
		false,
	)

	// Check that the config was created correctly
	if cfg.RedisAddr != "localhost:6379" {
		t.Errorf("Expected RedisAddr to be 'localhost:6379', got '%s'", cfg.RedisAddr)
	}

	if cfg.GitHubReleasesURL != "https://api.github.com/repos/librescoot/librescoot/releases" {
		t.Errorf("Expected GitHubReleasesURL to be 'https://api.github.com/repos/librescoot/librescoot/releases', got '%s'", cfg.GitHubReleasesURL)
	}

	if cfg.CheckInterval != 1*time.Hour {
		t.Errorf("Expected CheckInterval to be '1h0m0s', got '%s'", cfg.CheckInterval)
	}

	if cfg.DefaultChannel != "stable" {
		t.Errorf("Expected DefaultChannel to be 'stable', got '%s'", cfg.DefaultChannel)
	}

	if len(cfg.Components) != 2 {
		t.Errorf("Expected Components to have 2 elements, got %d", len(cfg.Components))
	}

	if cfg.Components[0] != "dbc" {
		t.Errorf("Expected Components[0] to be 'dbc', got '%s'", cfg.Components[0])
	}

	if cfg.Components[1] != "mdb" {
		t.Errorf("Expected Components[1] to be 'mdb', got '%s'", cfg.Components[1])
	}

	if cfg.DbcUpdateKey != "mender/update/dbc/url" {
		t.Errorf("Expected DbcUpdateKey to be 'mender/update/dbc/url', got '%s'", cfg.DbcUpdateKey)
	}

	if cfg.MdbUpdateKey != "mender/update/mdb/url" {
		t.Errorf("Expected MdbUpdateKey to be 'mender/update/mdb/url', got '%s'", cfg.MdbUpdateKey)
	}

	if cfg.DbcChecksumKey != "mender/update/dbc/checksum" {
		t.Errorf("Expected DbcChecksumKey to be 'mender/update/dbc/checksum', got '%s'", cfg.DbcChecksumKey)
	}

	if cfg.MdbChecksumKey != "mender/update/mdb/checksum" {
		t.Errorf("Expected MdbChecksumKey to be 'mender/update/mdb/checksum', got '%s'", cfg.MdbChecksumKey)
	}

	// Check that the constants are set correctly
	if OtaStatusHashKey != "ota" {
		t.Errorf("Expected OtaStatusHashKey constant to be 'ota', got '%s'", OtaStatusHashKey)
	}

	if VehicleHashKey != "vehicle" {
		t.Errorf("Expected VehicleHashKey constant to be 'vehicle', got '%s'", VehicleHashKey)
	}

	// Check default values for update constraints
	if cfg.MdbRebootCheckInterval != 5*time.Minute {
		t.Errorf("Expected MdbRebootCheckInterval to be '5m0s', got '%s'", cfg.MdbRebootCheckInterval)
	}

	if cfg.UpdateRetryInterval != 15*time.Minute {
		t.Errorf("Expected UpdateRetryInterval to be '15m0s', got '%s'", cfg.UpdateRetryInterval)
	}
}

func TestIsValidComponent(t *testing.T) {
	cfg := New(
		"localhost:6379",
		"https://api.github.com/repos/librescoot/librescoot/releases",
		1*time.Hour,
		"stable",
		"dbc,mdb",
		"mender/update/dbc/url",
		"mender/update/mdb/url",
		"mender/update/dbc/checksum",
		"mender/update/mdb/checksum",
		false,
	)

	// Test valid components
	if !cfg.IsValidComponent("dbc") {
		t.Errorf("Expected IsValidComponent('dbc') to be true")
	}

	if !cfg.IsValidComponent("mdb") {
		t.Errorf("Expected IsValidComponent('mdb') to be true")
	}

	// Test invalid component
	if cfg.IsValidComponent("invalid") {
		t.Errorf("Expected IsValidComponent('invalid') to be false")
	}
}

func TestIsValidChannel(t *testing.T) {
	cfg := New(
		"localhost:6379",
		"https://api.github.com/repos/librescoot/librescoot/releases",
		1*time.Hour,
		"stable",
		"dbc,mdb",
		"mender/update/dbc/url",
		"mender/update/mdb/url",
		"mender/update/dbc/checksum",
		"mender/update/mdb/checksum",
		false,
	)

	// Test valid channels
	if !cfg.IsValidChannel("stable") {
		t.Errorf("Expected IsValidChannel('stable') to be true")
	}

	if !cfg.IsValidChannel("testing") {
		t.Errorf("Expected IsValidChannel('testing') to be true")
	}

	if !cfg.IsValidChannel("nightly") {
		t.Errorf("Expected IsValidChannel('nightly') to be true")
	}

	// Test invalid channel
	if cfg.IsValidChannel("invalid") {
		t.Errorf("Expected IsValidChannel('invalid') to be false")
	}
}
