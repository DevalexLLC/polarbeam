package store

import "testing"

func TestChooseLatencySource(t *testing.T) {
	tests := []struct {
		name   string
		counts map[string]int64
		want   string
	}{
		{"empty", nil, ""},
		{"only zero counts", map[string]int64{"rtt": 0}, ""},
		{"single family", map[string]int64{"tcp_connect": 12}, "tcp_connect"},
		{"steady state rtt wins", map[string]int64{"rtt": 8000, "tcp_connect": 9000, "ttfb": 9000}, "rtt"},
		{"fresh rtt below coverage floor", map[string]int64{"rtt": 30, "tcp_connect": 8000}, "tcp_connect"},
		{"rtt at exactly 5% qualifies", map[string]int64{"rtt": 500, "tcp_connect": 9500}, "rtt"},
		{"floor skips to next family", map[string]int64{"rtt": 10, "tls_handshake": 20, "total": 9970}, "total"},
		{"nothing clears floor falls back to purest", map[string]int64{"rtt": 1, "tcp_connect": 1, "unknown_family": 98}, "rtt"},
		{"unknown families alone yield empty", map[string]int64{"unknown_family": 50}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := chooseLatencySource(tt.counts); got != tt.want {
				t.Errorf("chooseLatencySource(%v) = %q, want %q", tt.counts, got, tt.want)
			}
		})
	}
}
