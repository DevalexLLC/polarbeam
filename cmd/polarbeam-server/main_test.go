package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTokenCreateProblems(t *testing.T) {
	cases := []struct {
		name string
		site string
		ttl  time.Duration
		want []string
	}{
		{"valid", "site-a", 24 * time.Hour, nil},
		{"zero ttl", "site-a", 0, []string{"--ttl must be positive"}},
		{"negative ttl", "site-a", -time.Hour, []string{"--ttl must be positive"}},
		{"empty site", "", time.Hour, []string{"--site is required"}},
		{"empty site and negative ttl", "", -time.Hour, []string{"--site is required", "--ttl must be positive"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tokenCreateProblems(tc.site, tc.ttl)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("tokenCreateProblems(%q, %v) = %v, want %v", tc.site, tc.ttl, got, tc.want)
			}
		})
	}
}

func TestRetireCA(t *testing.T) {
	stamp := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	want := "ca.retired-20260909T120000Z"

	t.Run("renames and keeps contents", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "ca")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "ca.key"), []byte("k"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := retireCA(dir, stamp)
		if err != nil {
			t.Fatalf("retireCA: %v", err)
		}
		if got != filepath.Join(root, want) {
			t.Errorf("retired path = %s, want %s", got, filepath.Join(root, want))
		}
		if _, err := os.Stat(filepath.Join(got, "ca.key")); err != nil {
			t.Errorf("ca.key missing under retired dir: %v", err)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("original dir should be gone, stat err = %v", err)
		}
	})

	t.Run("trailing slash retires a sibling not a child", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "ca")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		got, err := retireCA(dir+string(filepath.Separator), stamp)
		if err != nil {
			t.Fatalf("retireCA with trailing slash: %v", err)
		}
		if got != filepath.Join(root, want) {
			t.Errorf("retired path = %s, want sibling %s", got, filepath.Join(root, want))
		}
	})

	t.Run("missing dir errors", func(t *testing.T) {
		_, err := retireCA(filepath.Join(t.TempDir(), "ca"), stamp)
		if err == nil || !strings.Contains(err.Error(), "no CA directory") {
			t.Errorf("want 'no CA directory' error, got %v", err)
		}
	})

	t.Run("existing target errors", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "ca")
		for _, d := range []string{dir, filepath.Join(root, want)} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := retireCA(dir, stamp); err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Errorf("want 'already exists' error, got %v", err)
		}
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("original dir must be untouched on refusal: %v", err)
		}
	})
}
