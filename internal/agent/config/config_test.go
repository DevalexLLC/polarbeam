package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const valid = `
server:
  address: polarbeam.example.com:443
`

func write(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadValidAppliesDefaults(t *testing.T) {
	cfg, err := Load(write(t, valid))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.StateDir != "/var/lib/polarbeam-agent" {
		t.Fatalf("default state_dir not applied: %q", cfg.StateDir)
	}
	if cfg.Spool.MaxBytes != 256<<20 || cfg.Spool.MaxAge != 7*24*time.Hour {
		t.Fatalf("spool defaults not applied: %+v", cfg.Spool)
	}
	if cfg.Server.UnreachableTimeout != 10*time.Minute {
		t.Fatalf("server.unreachable_timeout default not applied: %s", cfg.Server.UnreachableTimeout)
	}
}

func TestLoadUnreachableTimeout(t *testing.T) {
	cfg, err := Load(write(t, valid+"  unreachable_timeout: 0s\n"))
	if err != nil {
		t.Fatalf("0s (disabled) rejected: %v", err)
	}
	if cfg.Server.UnreachableTimeout != 0 {
		t.Fatalf("0s did not disable: %s", cfg.Server.UnreachableTimeout)
	}
	cfg, err = Load(write(t, valid+"  unreachable_timeout: 2m\n"))
	if err != nil {
		t.Fatalf("2m (the minimum) rejected: %v", err)
	}
	if cfg.Server.UnreachableTimeout != 2*time.Minute {
		t.Fatalf("2m not applied: %s", cfg.Server.UnreachableTimeout)
	}
	for _, v := range []string{"1m", "-1s"} {
		_, err := Load(write(t, valid+"  unreachable_timeout: "+v+"\n"))
		if err == nil || !strings.Contains(err.Error(), "server.unreachable_timeout") {
			t.Fatalf("%s not rejected by name: %v", v, err)
		}
	}
	// The docs promise `0s`, not `0`: yaml.v3 refuses a bare numeric for a
	// duration field, so a bare 0 must fail the load rather than disable.
	if _, err := Load(write(t, valid+"  unreachable_timeout: 0\n")); err == nil {
		t.Fatal("bare 0 accepted; docs say it is rejected")
	}
}

func TestLoadUnknownKeyNamed(t *testing.T) {
	_, err := Load(write(t, valid+"sppol:\n  max_bytes: 1\n"))
	if err == nil {
		t.Fatal("unknown key accepted")
	}
	if !strings.Contains(err.Error(), "sppol") {
		t.Fatalf("error does not name the key: %v", err)
	}
}

func TestLoadMissingServerAddress(t *testing.T) {
	_, err := Load(write(t, "log:\n  level: debug\n"))
	if err == nil || !strings.Contains(err.Error(), "server.address") {
		t.Fatalf("missing server.address not rejected by name: %v", err)
	}
}

func TestLoadNegativeSpoolBounds(t *testing.T) {
	_, err := Load(write(t, valid+"spool:\n  max_bytes: -1\n"))
	if err == nil || !strings.Contains(err.Error(), "spool.max_bytes") {
		t.Fatalf("negative spool.max_bytes not rejected by name: %v", err)
	}
}
