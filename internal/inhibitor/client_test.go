package inhibitor

import (
	"encoding/json"
	"log"
	"os"
	"testing"
	"time"

	ipc "github.com/librescoot/redis-ipc"
)

// The add/remove path only runs during an OTA, so it is easy to break
// without noticing. These tests pin the two things a consumer depends on:
// the hash entry and the notification payload, in that order.
func newTestClient(t *testing.T) (*Client, *ipc.Client) {
	t.Helper()
	raw, err := ipc.New(ipc.WithAddress("localhost"))
	if err != nil {
		t.Skipf("no local Redis: %v", err)
	}
	return New(raw, log.New(os.Stderr, "", 0)), raw
}

func TestAddInhibitWritesHashAndPublishes(t *testing.T) {
	c, raw := newTestClient(t)
	defer raw.Close()

	id := "test:add:" + time.Now().Format(time.RFC3339Nano)
	defer raw.Do("HDEL", InhibitHashKey, id)

	pubsub := raw.Raw().Subscribe(raw.Context(), InhibitChannel)
	defer pubsub.Close()
	ch := pubsub.Channel()
	time.Sleep(100 * time.Millisecond)

	if err := c.AddInhibit(id, "who", "what", "why", TypeDelay, 15*time.Second); err != nil {
		t.Fatalf("AddInhibit() failed: %v", err)
	}

	stored, err := raw.HGet(InhibitHashKey, id)
	if err != nil {
		t.Fatalf("HGet() failed: %v", err)
	}
	var got InhibitData
	if err := json.Unmarshal([]byte(stored), &got); err != nil {
		t.Fatalf("stored value is not InhibitData JSON: %v (%q)", err, stored)
	}
	if got.ID != id || got.Type != TypeDelay || got.Duration != 15 {
		t.Errorf("stored = %+v, want id=%s type=%s duration=15", got, id, TypeDelay)
	}

	select {
	case msg := <-ch:
		if want := "add:" + id; msg.Payload != want {
			t.Errorf("published %q, want %q", msg.Payload, want)
		}
	case <-time.After(2 * time.Second):
		t.Error("AddInhibit published nothing")
	}
}

func TestRemoveInhibitDeletesHashAndPublishes(t *testing.T) {
	c, raw := newTestClient(t)
	defer raw.Close()

	id := "test:rm:" + time.Now().Format(time.RFC3339Nano)
	if err := c.AddInhibit(id, "who", "what", "why", TypeBlock, 0); err != nil {
		t.Fatalf("AddInhibit() failed: %v", err)
	}

	pubsub := raw.Raw().Subscribe(raw.Context(), InhibitChannel)
	defer pubsub.Close()
	ch := pubsub.Channel()
	time.Sleep(100 * time.Millisecond)

	if err := c.RemoveInhibit(id); err != nil {
		t.Fatalf("RemoveInhibit() failed: %v", err)
	}

	if _, err := raw.HGet(InhibitHashKey, id); err != ipc.ErrNil {
		t.Errorf("HGet after remove = %v, want ErrNil", err)
	}

	select {
	case msg := <-ch:
		if want := "remove:" + id; msg.Payload != want {
			t.Errorf("published %q, want %q", msg.Payload, want)
		}
	case <-time.After(2 * time.Second):
		t.Error("RemoveInhibit published nothing")
	}
}
