package confcache

import (
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/types/known/durationpb"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
)

const agentA = "5e0f8a54-0000-0000-0000-000000000001"
const agentB = "5e0f8a54-0000-0000-0000-000000000002"

func snap(hash string, probes int) *pb.ConfigSnapshot {
	s := &pb.ConfigSnapshot{ConfigHash: hash}
	for i := 0; i < probes; i++ {
		s.Probes = append(s.Probes, &pb.ProbeSpec{
			ProbeId:  string(rune('a' + i)),
			Type:     pb.ProbeType_PROBE_TYPE_TCP,
			Interval: durationpb.New(30_000_000_000),
		})
	}
	return s
}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()

	// First run: no cache is not an error.
	got, err := Load(dir, agentA)
	if err != nil || got != nil {
		t.Fatalf("fresh dir: Load = (%v, %v), want (nil, nil)", got, err)
	}

	want := snap("h1", 3)
	if err := Store(dir, agentA, want); err != nil {
		t.Fatalf("Store: %v", err)
	}
	got, err = Load(dir, agentA)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.GetConfigHash() != "h1" || len(got.GetProbes()) != 3 {
		t.Errorf("round trip: hash=%q probes=%d, want h1/3", got.GetConfigHash(), len(got.GetProbes()))
	}

	// Overwrite wins.
	if err := Store(dir, agentA, snap("h2", 1)); err != nil {
		t.Fatalf("Store h2: %v", err)
	}
	if got, _ = Load(dir, agentA); got.GetConfigHash() != "h2" {
		t.Errorf("after overwrite: hash = %q, want h2", got.GetConfigHash())
	}
}

// TestIdentityBinding: a re-enrolled agent (new UUID) must never load the
// previous identity's cache — and writing its own cache sweeps the old one.
func TestIdentityBinding(t *testing.T) {
	dir := t.TempDir()
	if err := Store(dir, agentA, snap("old-identity", 2)); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir, agentB)
	if err != nil || got != nil {
		t.Fatalf("other identity's cache: Load = (%v, %v), want (nil, nil)", got, err)
	}
	if err := Store(dir, agentB, snap("new-identity", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cachePath(dir, agentA)); !os.IsNotExist(err) {
		t.Error("old identity's cache survived the new identity's store")
	}
}

func TestCorruptionIsAnError(t *testing.T) {
	dir := t.TempDir()

	for name, contents := range map[string][]byte{
		"empty file":        {},
		"short of checksum": []byte("tiny"),
	} {
		if err := os.WriteFile(cachePath(dir, agentA), contents, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(dir, agentA); err == nil {
			t.Errorf("%s: Load returned nil error, want loud failure", name)
		}
	}

	// A flipped payload byte fails the checksum even when the result would
	// still be wire-valid protobuf.
	if err := Store(dir, agentA, snap("h1", 2)); err != nil {
		t.Fatal(err)
	}
	path := cachePath(dir, agentA)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b[2] ^= 0xff
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, agentA); err == nil {
		t.Error("bit-flipped cache: Load returned nil error, want checksum failure")
	}

	// A checksum-valid snapshot WITHOUT a config hash is not something the
	// server ever sent: rejected.
	if err := Store(dir, agentA, &pb.ConfigSnapshot{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, agentA); err == nil {
		t.Error("hashless snapshot accepted")
	}
}

func TestStoreIsAtomicAndClearIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := Store(dir, agentA, snap("h1", 1)); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(cachePath(dir, agentA)) {
		t.Errorf("dir contents = %v, want exactly the cache file", entries)
	}
	if err := Clear(dir, agentA); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if err := Clear(dir, agentA); err != nil {
		t.Fatalf("Clear on empty: %v", err)
	}
}
