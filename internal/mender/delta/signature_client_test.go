package delta

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Exercise the real client's signature and payload checks with a disposable
// module that writes only into its temporary directory, never a rootfs slot.
func verifySigningTestClient(t *testing.T, artifact, public string, valid bool) {
	t.Helper()
	client := os.Getenv("MENDER_UPDATE")
	if client == "" {
		return
	}
	dir := t.TempDir()
	conf, data, state := filepath.Join(dir, "conf"), filepath.Join(dir, "data"), filepath.Join(dir, "state")
	module := filepath.Join(data, "modules", "v3", "rootfs-image")
	for _, path := range []string{conf, state, filepath.Dir(module)} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	config := map[string]string{}
	if public != "" {
		config["ArtifactVerifyKey"] = public
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(conf, "mender.conf"):  string(encoded),
		filepath.Join(state, "mender.conf"): "{}",
		filepath.Join(state, "device_type"): "device_type=signing-test\n",
		module: `#!/bin/sh
set -eu
printf '%s\n' "$1" >> "$MENDER_DATA_DIR/module-states"
case "$1" in
    ProvidePayloadFileSizes) echo Yes ;;
    DownloadWithFileSizes)
        line="$(cat stream-next)"
        file="${line% *}"
        cat "$file" > "$MENDER_DATA_DIR/payload"
        test "$(cat stream-next)" = ""
        ;;
    NeedsArtifactReboot|SupportsRollback) echo No ;;
esac
`,
	}
	for path, body := range files {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(module, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, client, "--config", filepath.Join(conf, "mender.conf"),
		"--datastore", state, "install", artifact)
	cmd.Env = append(os.Environ(), "MENDER_CONF_DIR="+conf, "MENDER_DATA_DIR="+data, "MENDER_DATASTORE_DIR="+state)
	out, err := cmd.CombinedOutput()
	if (err == nil) != valid || ctx.Err() != nil {
		t.Fatalf("client install %s (valid=%t): %v\n%s", artifact, valid, err, out)
	}
	states, err := os.ReadFile(filepath.Join(data, "module-states"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	installed := strings.Contains(string(states), "ArtifactInstall\n")
	if installed != valid {
		t.Fatalf("client activation=%t, want %t; module states: %s\n%s", installed, valid, states, out)
	}
}
