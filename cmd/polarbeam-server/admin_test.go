package main

import (
	"testing"
)

func TestParamsFlag(t *testing.T) {
	p := paramsFlag{}
	if err := p.Set("http.expect_status=200"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := p.Set("port=5432"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if p["http.expect_status"] != "200" || p["port"] != "5432" {
		t.Errorf("params = %v", p)
	}
	// Values may themselves contain '=': only the first split counts.
	if err := p.Set("k=a=b"); err != nil || p["k"] != "a=b" {
		t.Errorf("Set(k=a=b): err=%v val=%q", err, p["k"])
	}
	for _, bad := range []string{"noequals", "=value", ""} {
		if err := p.Set(bad); err == nil {
			t.Errorf("Set(%q): expected error", bad)
		}
	}
}

// Probe type parsing and cadence/train validation moved to
// internal/server/probeadmin (shared with the HTTP config API) and are
// tested there. Site coordinate validation likewise moved to
// internal/server/siteadmin.
