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
