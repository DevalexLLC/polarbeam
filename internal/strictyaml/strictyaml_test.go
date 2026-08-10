package strictyaml

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type testCfg struct {
	Name  string `yaml:"name"`
	Count int    `yaml:"count"`
}

func write(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cfg.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadFileValid(t *testing.T) {
	var c testCfg
	if err := LoadFile(write(t, "name: a\ncount: 2\n"), &c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Name != "a" || c.Count != 2 {
		t.Fatalf("bad decode: %+v", c)
	}
}

func TestLoadFileUnknownKeyIsFatalAndNamed(t *testing.T) {
	var c testCfg
	err := LoadFile(write(t, "name: a\nnmae_typo: b\n"), &c)
	if err == nil {
		t.Fatal("unknown key accepted")
	}
	if !strings.Contains(err.Error(), "nmae_typo") {
		t.Fatalf("error does not name the offending key: %v", err)
	}
}

func TestLoadFileTypeMismatchIsFatal(t *testing.T) {
	var c testCfg
	if err := LoadFile(write(t, "count: notanumber\n"), &c); err == nil {
		t.Fatal("type mismatch accepted")
	}
}

func TestLoadFileEmptyIsFatal(t *testing.T) {
	var c testCfg
	if err := LoadFile(write(t, ""), &c); err == nil {
		t.Fatal("empty file accepted")
	}
}

func TestLoadFileMultiDocIsFatal(t *testing.T) {
	var c testCfg
	if err := LoadFile(write(t, "name: a\n---\nname: b\n"), &c); err == nil {
		t.Fatal("second document silently ignored")
	}
}

func TestLoadFileMissingFile(t *testing.T) {
	var c testCfg
	if err := LoadFile("/nonexistent/cfg.yaml", &c); err == nil {
		t.Fatal("missing file accepted")
	}
}
