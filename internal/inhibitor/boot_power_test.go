package inhibitor

import (
	"context"
	"io"
	"log"
	"testing"

	"github.com/alicebob/miniredis/v2"
	ipc "github.com/librescoot/redis-ipc"
)

func TestBootPowerAcknowledgement(t *testing.T) {
	server := miniredis.RunT(t)
	raw, err := ipc.New(ipc.WithURL("redis://" + server.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	client := New(raw, log.New(io.Discard, "", 0))
	server.HSet("power-manager", "state", "running")
	if err := client.AddBootInstallInhibit("dbc", "new-request"); err != nil {
		t.Fatal(err)
	}
	// Redis accepting HSET is not evidence that pm-service processed it.
	if ready, err := client.BootInstallObserved(context.Background(), "dbc", "new-request"); ready || err != nil {
		t.Fatalf("ready=%v err=%v", ready, err)
	}
	server.HSet(BootBusyServicesHash, BootInstallAcknowledgementField("dbc", "old-request"), "block")
	if ready, err := client.BootInstallObserved(context.Background(), "dbc", "new-request"); ready || err != nil {
		t.Fatalf("stale ack: ready=%v err=%v", ready, err)
	}
	field := BootInstallAcknowledgementField("dbc", "new-request")
	if field != "update-service installing boot update for dbc (new-request) power-state-change" {
		t.Fatal(field)
	}
	server.HSet(BootBusyServicesHash, field, "block")
	if ready, err := client.BootInstallObserved(context.Background(), "dbc", "new-request"); !ready || err != nil {
		t.Fatalf("ready=%v err=%v", ready, err)
	}
	server.HSet(BootBusyServicesHash, field, "delay")
	if ready, err := client.BootInstallObserved(context.Background(), "dbc", "new-request"); ready || err == nil {
		t.Fatalf("delay: ready=%v err=%v", ready, err)
	}
	server.HSet(BootBusyServicesHash, field, "block")
	for _, state := range []string{"", "unknown", "suspending-pending", "suspending-imminent", "hibernating-imminent", "reboot-imminent", "suspending", "hibernating", "hibernating-manual", "hibernating-timer", "hibernating-for", "reboot", "poweroff"} {
		server.HSet("power-manager", "state", state)
		if ready, err := client.BootInstallObserved(context.Background(), "dbc", "new-request"); ready || err == nil {
			t.Fatalf("state=%q ready=%v err=%v", state, ready, err)
		}
	}
	server.HSet("power-manager", "state", "running")
	server.SetError("ERR unavailable")
	if ready, err := client.BootInstallObserved(context.Background(), "dbc", "new-request"); ready || err == nil {
		t.Fatalf("Redis error: ready=%v err=%v", ready, err)
	}
}
