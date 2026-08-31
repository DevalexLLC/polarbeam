package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// DirectProbeRow is a direct (site-scoped) probe assignment for one agent.
// DstSiteID is the site of an agent-kind target (nil for external targets);
// ingest uses it to resolve per-pair threshold overrides. It never enters
// the config snapshot, so it cannot perturb config_hash.
type DirectProbeRow struct {
	ID        uuid.UUID
	Settings  ProbeSettings
	TargetID  uuid.UUID
	Kind      string
	Address   string
	Port      int32
	URL       string
	DstSiteID *uuid.UUID
}

// MeshProbeRow is a mesh probe template applying to the agent's site.
// ConfigID is the template's probe_configs.id — the namespace of every
// probe ID the template expands to.
type MeshProbeRow struct {
	ConfigID uuid.UUID
	MeshID   uuid.UUID
	Settings ProbeSettings
}

// PeerRow is a peer agent reachable through a shared mesh.
type PeerRow struct {
	MeshID       uuid.UUID
	AgentID      uuid.UUID
	SiteID       uuid.UUID
	TargetID     uuid.UUID // the peer's agent-kind targets.id
	ProbeAddress string
}

// AgentConfigInputs is everything needed to expand one agent's config
// snapshot (consumed by meshexpand, which is pure).
type AgentConfigInputs struct {
	AgentID uuid.UUID
	SiteID  uuid.UUID
	// NetworkID is the agent's plane. meshexpand never reads it — the SQL
	// below already scopes every row — but ingest needs it to resolve the
	// plane-qualified threshold layers for the pairs this agent measures.
	NetworkID uuid.UUID
	Direct    []DirectProbeRow
	Mesh      []MeshProbeRow
	Peers     []PeerRow
}

// LoadAgentConfigInputs gathers the agent's site, its site's direct probes,
// mesh templates covering the site, and mesh peers — one batched round trip.
// Everything is scoped to the agent's network in the SQL itself (direct
// probes match on (site, network); mesh templates and peers only where the
// mesh's network matches), so meshexpand stays network-ignorant and the
// ingest allowlist tightens for free.
func (s *Store) LoadAgentConfigInputs(ctx context.Context, agentID uuid.UUID) (AgentConfigInputs, error) {
	in := AgentConfigInputs{AgentID: agentID}

	batch := &pgx.Batch{}
	batch.Queue(`SELECT site_id, network_id FROM agents WHERE id = $1`, agentID)
	batch.Queue(`
		SELECT pc.id, pc.probe_type, pc.interval_ms, pc.timeout_ms, pc.train_count, pc.train_spacing_ms, pc.params,
		       t.id, t.kind, t.address, t.port, t.url, dta.site_id
		FROM probe_configs pc
		JOIN targets t ON t.id = pc.target_id
		LEFT JOIN agents dta ON dta.id = t.agent_id
		JOIN agents a ON a.site_id = pc.site_id AND a.network_id = pc.network_id
		WHERE a.id = $1 AND pc.enabled
		ORDER BY pc.created_at`, agentID)
	batch.Queue(`
		SELECT pc.id, pc.mesh_id, pc.probe_type, pc.interval_ms, pc.timeout_ms, pc.train_count, pc.train_spacing_ms, pc.params
		FROM probe_configs pc
		JOIN mesh_groups mg ON mg.id = pc.mesh_id
		JOIN mesh_members mm ON mm.mesh_id = pc.mesh_id
		JOIN agents a ON a.site_id = mm.site_id AND a.network_id = mg.network_id
		WHERE a.id = $1 AND pc.enabled
		ORDER BY pc.created_at`, agentID)
	batch.Queue(`
		SELECT DISTINCT mine.mesh_id, peer.id, peer.site_id, t.id, peer.probe_address
		FROM agents me
		JOIN mesh_members mine ON mine.site_id = me.site_id
		JOIN mesh_groups mg ON mg.id = mine.mesh_id AND mg.network_id = me.network_id
		JOIN mesh_members theirs ON theirs.mesh_id = mine.mesh_id AND theirs.site_id <> mine.site_id
		JOIN agents peer ON peer.site_id = theirs.site_id AND peer.network_id = mg.network_id
		JOIN targets t ON t.agent_id = peer.id
		WHERE me.id = $1
		ORDER BY mine.mesh_id, peer.id, t.id`, agentID)

	res := s.pool.SendBatch(ctx, batch)
	defer res.Close()

	if err := res.QueryRow().Scan(&in.SiteID, &in.NetworkID); err != nil {
		return in, fmt.Errorf("load config inputs: agent %s: %w", agentID, err)
	}

	rows, err := res.Query()
	if err != nil {
		return in, fmt.Errorf("load config inputs: direct probes: %w", err)
	}
	for rows.Next() {
		var (
			d                                     DirectProbeRow
			intervalMS, timeoutMS, trainSpacingMS int64
		)
		if err := rows.Scan(&d.ID, &d.Settings.ProbeType, &intervalMS, &timeoutMS,
			&d.Settings.TrainCount, &trainSpacingMS, &d.Settings.Params,
			&d.TargetID, &d.Kind, &d.Address, &d.Port, &d.URL, &d.DstSiteID); err != nil {
			rows.Close()
			return in, fmt.Errorf("load config inputs: direct probes: %w", err)
		}
		d.Settings.Interval = time.Duration(intervalMS) * time.Millisecond
		d.Settings.Timeout = time.Duration(timeoutMS) * time.Millisecond
		d.Settings.TrainSpacing = time.Duration(trainSpacingMS) * time.Millisecond
		in.Direct = append(in.Direct, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return in, fmt.Errorf("load config inputs: direct probes: %w", err)
	}

	rows, err = res.Query()
	if err != nil {
		return in, fmt.Errorf("load config inputs: mesh probes: %w", err)
	}
	for rows.Next() {
		var (
			m                                     MeshProbeRow
			intervalMS, timeoutMS, trainSpacingMS int64
		)
		if err := rows.Scan(&m.ConfigID, &m.MeshID, &m.Settings.ProbeType, &intervalMS, &timeoutMS,
			&m.Settings.TrainCount, &trainSpacingMS, &m.Settings.Params); err != nil {
			rows.Close()
			return in, fmt.Errorf("load config inputs: mesh probes: %w", err)
		}
		m.Settings.Interval = time.Duration(intervalMS) * time.Millisecond
		m.Settings.Timeout = time.Duration(timeoutMS) * time.Millisecond
		m.Settings.TrainSpacing = time.Duration(trainSpacingMS) * time.Millisecond
		in.Mesh = append(in.Mesh, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return in, fmt.Errorf("load config inputs: mesh probes: %w", err)
	}

	rows, err = res.Query()
	if err != nil {
		return in, fmt.Errorf("load config inputs: peers: %w", err)
	}
	for rows.Next() {
		var p PeerRow
		if err := rows.Scan(&p.MeshID, &p.AgentID, &p.SiteID, &p.TargetID, &p.ProbeAddress); err != nil {
			rows.Close()
			return in, fmt.Errorf("load config inputs: peers: %w", err)
		}
		in.Peers = append(in.Peers, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return in, fmt.Errorf("load config inputs: peers: %w", err)
	}

	return in, nil
}
