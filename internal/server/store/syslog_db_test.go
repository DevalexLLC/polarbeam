package store_test

import (
	"testing"
	"time"

	"github.com/devalexllc/polarbeam/internal/server/store"
)

// TestSyslogSettingsRoundTrip: the seeded defaults, a full update, and the
// write-only client key convention.
func TestSyslogSettingsRoundTrip(t *testing.T) {
	ctx, s := newStore(t)

	cur, err := s.GetSyslogSettings(ctx)
	if err != nil {
		t.Fatalf("GetSyslogSettings: %v", err)
	}
	if cur.Enabled || cur.Transport != "tls" || cur.Port != 6514 || cur.Framing != "octet-counted" ||
		cur.Facility != 16 || cur.Content != "all" || cur.MinLevel != "info" || cur.OnFailure != "warn" ||
		cur.FailureTimeout != 5*time.Minute {
		t.Fatalf("seeded row = %+v", cur)
	}

	in := store.SyslogSettings{
		Enabled: true, Transport: "tcp", Host: "collector.example", Port: 601, Framing: "non-transparent",
		Facility: 10, Hostname: "polarbeam.example", Content: "audit", MinLevel: "warn", OnFailure: "halt",
		FailureTimeout: 2 * time.Minute, TLSCAPEM: "ca", TLSClientCertPEM: "cert", TLSClientKeyPEM: "key",
		TLSServerName: "collector", UpdatedBy: "root",
	}
	out, err := s.UpdateSyslogSettings(ctx, in, false)
	if err != nil {
		t.Fatalf("UpdateSyslogSettings: %v", err)
	}
	if out.Host != "collector.example" || out.Port != 601 || out.Facility != 10 || out.FailureTimeout != 2*time.Minute ||
		out.TLSClientKeyPEM != "key" || out.UpdatedBy != "root" || out.UpdatedAt.Before(cur.UpdatedAt) {
		t.Fatalf("stored row = %+v", out)
	}

	// keepClientKey: the submitted (empty) key is ignored, the stored one stays.
	in.Host, in.TLSClientKeyPEM = "other.example", ""
	out, err = s.UpdateSyslogSettings(ctx, in, true)
	if err != nil {
		t.Fatalf("UpdateSyslogSettings keep: %v", err)
	}
	if out.Host != "other.example" || out.TLSClientKeyPEM != "key" {
		t.Errorf("keep-key row = host %q key %q", out.Host, out.TLSClientKeyPEM)
	}
	// Explicit clearing.
	in.TLSClientCertPEM, in.TLSClientKeyPEM = "", ""
	if out, err = s.UpdateSyslogSettings(ctx, in, false); err != nil || out.TLSClientKeyPEM != "" {
		t.Errorf("clear key: err=%v key=%q", err, out.TLSClientKeyPEM)
	}

	// The CHECKs guard what the API validates.
	in.Enabled, in.Host = true, ""
	if _, err := s.UpdateSyslogSettings(ctx, in, false); err == nil {
		t.Error("enabled with an empty host was accepted")
	}
}
