package power

import (
	"log"
	"os"
	"testing"
	"time"

	ipc "github.com/librescoot/redis-ipc"
)

// RequestOndemandGovernor is fire-and-forget from update-service's side,
// so a broken LPUSH would go unnoticed until someone wondered why delta
// application was running at powersave clocks.
func TestRequestOndemandGovernorPushes(t *testing.T) {
	raw, err := ipc.New(ipc.WithAddress("localhost"))
	if err != nil {
		t.Skipf("no local Redis: %v", err)
	}
	defer raw.Close()
	defer raw.Del(PowerGovernorListKey)

	if _, err := raw.Del(PowerGovernorListKey); err != nil {
		t.Fatalf("Del() failed: %v", err)
	}

	if err := New(raw, log.New(os.Stderr, "", 0)).RequestOndemandGovernor(); err != nil {
		t.Fatalf("RequestOndemandGovernor() failed: %v", err)
	}

	vals, err := raw.BRPop(2*time.Second, PowerGovernorListKey)
	if err != nil {
		t.Fatalf("BRPop() failed: %v", err)
	}
	if len(vals) < 2 || vals[1] != "ondemand" {
		t.Errorf("popped %v, want [%s ondemand]", vals, PowerGovernorListKey)
	}
}
