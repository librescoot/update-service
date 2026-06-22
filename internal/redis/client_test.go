package redis

import (
	"testing"
	"time"
)

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
