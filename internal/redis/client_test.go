package redis

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func newTestClient(t *testing.T) (*Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client, err := New(mr.Addr())
	if err != nil {
		t.Fatalf("connecting test client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, mr
}

func TestCheckAttemptFields(t *testing.T) {
	client, mr := newTestClient(t)

	attempt := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	if err := client.SetLastAttemptTime("mdb", attempt); err != nil {
		t.Fatalf("SetLastAttemptTime: %v", err)
	}
	if err := client.SetLastAttemptResult("mdb", "running"); err != nil {
		t.Fatalf("SetLastAttemptResult: %v", err)
	}

	if got := mr.HGet("settings", "updates.mdb.last-attempt-time"); got != attempt.Format(time.RFC3339) {
		t.Errorf("last-attempt-time = %q, want %q", got, attempt.Format(time.RFC3339))
	}
	if got := mr.HGet("settings", "updates.mdb.last-attempt-result"); got != "running" {
		t.Errorf("last-attempt-result = %q, want running", got)
	}
}

func TestLastUpdateCheckTimeRoundTrip(t *testing.T) {
	client, _ := newTestClient(t)

	completed := time.Date(2026, 9, 18, 10, 5, 0, 0, time.UTC)
	if err := client.SetLastUpdateCheckTime("dbc", completed); err != nil {
		t.Fatalf("SetLastUpdateCheckTime: %v", err)
	}

	got, err := client.GetLastUpdateCheckTime("dbc")
	if err != nil {
		t.Fatalf("GetLastUpdateCheckTime: %v", err)
	}
	if !got.Equal(completed) {
		t.Errorf("GetLastUpdateCheckTime = %v, want %v", got, completed)
	}
}

func TestParseVehicleTimestamp(t *testing.T) {
	rfc := "2026-06-22T08:49:55+02:00"
	rfcWant, _ := time.Parse(time.RFC3339, rfc)

	tests := []struct {
		name string
		in   string
		want time.Time
	}{
		{"empty", "", time.Time{}},
		{"rfc3339", rfc, rfcWant},
		{"unix millis (redis-ipc <= v0.10)", "1750574995000", time.UnixMilli(1750574995000)},
		{"garbage", "not-a-time", time.Time{}},
		{"zero millis", "0", time.Time{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseVehicleTimestamp(tt.in)
			if !got.Equal(tt.want) {
				t.Errorf("parseVehicleTimestamp(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
