package updater

import (
	"context"
	"errors"
	"testing"

	"github.com/librescoot/update-service/internal/mender"
)

func TestBootWaitsForProcessedPowerBlock(t *testing.T) {
	f := newFakeBoot()
	d := f.deps()
	original := d.powerObserved
	reads := 0
	d.powerObserved = func(ctx context.Context, token string) (bool, error) {
		reads++
		if f.writes != 0 {
			t.Fatal("write before power observation")
		}
		if reads == 1 {
			return false, nil
		}
		return original(ctx, token)
	}
	applied, err := runLocalBootUpdate(context.Background(), "dbc", d)
	if !applied || err != nil || reads != 3 {
		t.Fatalf("applied=%v err=%v reads=%d", applied, err, reads)
	}
}

func TestBootPowerObservationFailsClosed(t *testing.T) {
	for _, mode := range []string{"timeout", "redis-error", "cancel", "lost-before-write", "state-changed"} {
		t.Run(mode, func(t *testing.T) {
			f := newFakeBoot()
			d := f.deps()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			original := d.powerObserved
			reads := 0
			d.powerObserved = func(ctx context.Context, token string) (bool, error) {
				reads++
				switch mode {
				case "timeout":
					return false, nil
				case "redis-error":
					return false, errBootTest
				case "cancel":
					cancel()
					return false, ctx.Err()
				case "lost-before-write":
					if reads > 1 {
						return false, nil
					}
				case "state-changed":
					if reads > 1 {
						return false, errBootTest
					}
				}
				return original(ctx, token)
			}
			applied, err := runLocalBootUpdate(ctx, "dbc", d)
			if applied || err == nil || f.writes != 0 || f.held {
				t.Fatalf("applied=%v err=%v writes=%d held=%v", applied, err, f.writes, f.held)
			}
			if mode == "timeout" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
			if mode == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			started, complete := false, false
			for _, event := range f.events {
				started = started || event == "start-dbc"
				complete = complete || event == "complete-dbc"
			}
			if started != complete {
				t.Fatal(f.events)
			}
		})
	}
}

func TestBootStartupCleanupBeforeNoOp(t *testing.T) {
	for _, mode := range []string{"no-assets", "already-current", "pending-rootfs"} {
		t.Run(mode, func(t *testing.T) {
			f := newFakeBoot()
			f.held = true
			if err := cleanupBootUpdate(f, "mdb"); err != nil {
				t.Fatal(err)
			}
			// Start runs cleanup unconditionally, even if no boot updater/assets exist.
			if mode != "no-assets" {
				if mode == "already-current" {
					f.current = true
				} else {
					f.state = mender.StateNeedsReboot
				}
				applied, _ := runLocalBootUpdate(context.Background(), "mdb", f.deps())
				if applied {
					t.Fatal("unexpected write")
				}
			}
			if f.held || f.writes != 0 || len(f.events) != 1 || f.events[0] != "release" {
				t.Fatalf("events=%v held=%v", f.events, f.held)
			}
		})
	}
}

func TestBootPowerRequestChangesBetweenOperations(t *testing.T) {
	f := newFakeBoot()
	if _, err := runLocalBootUpdate(context.Background(), "mdb", f.deps()); err != nil {
		t.Fatal(err)
	}
	previous := f.requestID
	if _, err := runLocalBootUpdate(context.Background(), "mdb", f.deps()); err != nil {
		t.Fatal(err)
	}
	if previous == f.requestID {
		t.Fatal("reused power acknowledgement request")
	}
}
