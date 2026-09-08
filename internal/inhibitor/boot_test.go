package inhibitor

import (
	"encoding/json"
	"io"
	"log"
	"testing"

	"github.com/alicebob/miniredis/v2"
	ipc "github.com/librescoot/redis-ipc"
)

func TestBootInstallInhibitIsPersistentBlock(t *testing.T) {
	server := miniredis.RunT(t)
	raw, err := ipc.New(ipc.WithURL("redis://" + server.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	client := New(raw, log.New(io.Discard, "", 0))
	for _, component := range []string{"mdb", "dbc"} {
		t.Run(component, func(t *testing.T) {
			if err := client.AddBootInstallInhibit(component, "test-request"); err != nil {
				t.Fatal(err)
			}
			key := "install:" + component + "-boot"
			var got InhibitData
			if err := json.Unmarshal([]byte(server.HGet(InhibitHashKey, key)), &got); err != nil {
				t.Fatal(err)
			}
			if got.ID != key || got.Type != TypeBlock || got.Duration != 0 {
				t.Fatalf("inhibit=%+v", got)
			}
			if err := client.RemoveBootInstallInhibit(component); err != nil {
				t.Fatal(err)
			}
			if server.HGet(InhibitHashKey, key) != "" {
				t.Fatal("inhibitor leaked")
			}
		})
	}
}

func TestBootInstallInhibitRedisError(t *testing.T) {
	server := miniredis.RunT(t)
	raw, err := ipc.New(ipc.WithURL("redis://" + server.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	client := New(raw, log.New(io.Discard, "", 0))
	server.SetError("ERR unavailable")
	if err := client.AddBootInstallInhibit("dbc", "test-request"); err == nil {
		t.Fatal("acquisition must fail closed")
	}
}
