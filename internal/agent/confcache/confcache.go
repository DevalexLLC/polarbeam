// Package confcache persists the last applied config snapshot beside the
// spool. Without it, config lived only in RAM: an agent restarted while the
// control plane was unreachable (host reboot, container restart, fatal
// spool-error exit during the same WAN outage) probed NOTHING until the
// server came back — the spool preserved results already produced, but not
// the ability to produce them. On startup the cached snapshot is applied
// immediately and its hash rides AgentHello, so a reachable server that has
// moved on still re-sends in the usual way; the cache never overrides the
// server, it only covers its absence.
//
// The file is bound to the agent identity (the UUID from the enrolled
// certificate) in its name: the documented re-enrollment flow wipes only
// the pki directory, and a cache left by the previous identity must never
// drive the new one's probes. Content integrity is a sha256 trailer over
// the marshaled snapshot — protobuf tolerates too much (an empty file
// unmarshals to an empty snapshot; a flipped byte can survive as different
// wire-valid content whose stored hash would then suppress the server's
// corrective send).
package confcache

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"google.golang.org/protobuf/proto"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
)

func cachePath(stateDir, agentID string) string {
	return filepath.Join(stateDir, "config-cache-"+agentID+".pb")
}

// Load returns the agent's cached snapshot, or (nil, nil) when none has
// ever been written for THIS identity. Corruption — a bad checksum, or a
// snapshot without a config hash — is an error: callers log it loudly and
// proceed without a cache, exactly as on first run.
func Load(stateDir, agentID string) (*pb.ConfigSnapshot, error) {
	b, err := os.ReadFile(cachePath(stateDir, agentID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("config cache: %w", err)
	}
	if len(b) < sha256.Size {
		return nil, errors.New("config cache: truncated")
	}
	payload, sum := b[:len(b)-sha256.Size], b[len(b)-sha256.Size:]
	if got := sha256.Sum256(payload); !bytes.Equal(got[:], sum) {
		return nil, errors.New("config cache: checksum mismatch")
	}
	snap := &pb.ConfigSnapshot{}
	if err := proto.Unmarshal(payload, snap); err != nil {
		return nil, fmt.Errorf("config cache: unmarshal: %w", err)
	}
	if snap.GetConfigHash() == "" {
		// Every server-built snapshot carries a hash; its absence means
		// the payload is not a snapshot this agent ever received.
		return nil, errors.New("config cache: snapshot has no config hash")
	}
	return snap, nil
}

// Store atomically persists a snapshot for the agent identity (tmp +
// rename, like the spool's sidecars): a crash mid-write leaves the previous
// cache intact, never a torn file. Caches left behind by previous
// identities (re-enrollment) are swept best-effort.
func Store(stateDir, agentID string, snap *pb.ConfigSnapshot) error {
	payload, err := proto.Marshal(snap)
	if err != nil {
		return fmt.Errorf("config cache: marshal: %w", err)
	}
	sum := sha256.Sum256(payload)
	path := cachePath(stateDir, agentID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(payload, sum[:]...), 0o600); err != nil {
		return fmt.Errorf("config cache: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("config cache: %w", err)
	}
	sweepForeign(stateDir, agentID)
	return nil
}

// Clear removes the agent's cache. Called when persisting a NEWER snapshot
// failed: a stale cache that survives a failed update is worse than none —
// the next restart would resurrect an old schedule, and the reconnect path
// cannot repair it (the uplink already advertises the newer hash, so the
// server skips the resend).
func Clear(stateDir, agentID string) error {
	err := os.Remove(cachePath(stateDir, agentID))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// sweepForeign deletes caches bound to other identities. Best-effort: a
// leftover foreign cache is inert (Load is identity-keyed), this is just
// hygiene after a re-enrollment.
func sweepForeign(stateDir, agentID string) {
	matches, err := filepath.Glob(filepath.Join(stateDir, "config-cache-*.pb"))
	if err != nil {
		return
	}
	keep := cachePath(stateDir, agentID)
	for _, m := range matches {
		if m != keep {
			os.Remove(m)
		}
	}
}
