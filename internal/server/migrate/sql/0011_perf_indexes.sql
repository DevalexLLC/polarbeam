-- Supporting indexes for hot read paths, FK scans, and cleanup scans.
-- Plain B-trees only — no table rewrites, so each build is a brief SHARE
-- lock. Names follow the existing <table>_<col>_idx convention.
--
-- Ordering matters: the documented live upgrade (docs/install.md) runs
-- `migrate` while the old server is still serving, and every SHARE lock
-- this transaction takes is held until commit. The potentially large
-- continuous-aggregate builds therefore come FIRST (their SHARE locks
-- only pause cagg refresh jobs, which rerun), and the tables the ingest
-- and auth paths write — series_state, outage_events, sessions — are
-- locked LAST, so their small builds block writes only for the final
-- moments before commit.

-- Dashboard pair queries filter both caggs on agent_id + target_id +
-- bucket; TimescaleDB's auto-created indexes are single-key. These land
-- on the materialization hypertables, so the write cost falls on cagg
-- refresh, not raw ingest.
CREATE INDEX probe_results_hourly_pair_idx
    ON probe_results_hourly (agent_id, target_id, bucket DESC);
CREATE INDEX probe_results_daily_pair_idx
    ON probe_results_daily (agent_id, target_id, bucket DESC);

-- Config snapshot statements (LoadAgentConfigInputs) join probe_configs
-- on site_id / mesh_id every 30 s per connected agent; target_id serves
-- FK checks on target deletion. Exactly one of site/mesh is set per row,
-- but plain indexes keep this simple: B-trees index NULLs cheaply and
-- the table stays small.
CREATE INDEX probe_configs_site_idx   ON probe_configs (site_id);
CREATE INDEX probe_configs_target_idx ON probe_configs (target_id);
CREATE INDEX probe_configs_mesh_idx   ON probe_configs (mesh_id);

-- Snapshot statements enter mesh_members via mm.site_id; the PK
-- (mesh_id, site_id) has no leading site_id. Including mesh_id makes the
-- reverse index covering, so those joins are index-only scans.
CREATE INDEX mesh_members_site_idx ON mesh_members (site_id, mesh_id);

-- Unindexed FK child column: site-deletion reference checks.
-- join_tokens.used_by_agent stays unindexed on purpose — no agent-delete
-- path exists.
CREATE INDEX join_tokens_site_idx ON join_tokens (site_id);

-- cleanupSeries deletes/updates by probe_id across agents (a mesh change
-- expands to many concrete probe IDs); the PK leads with agent_id and
-- cannot serve it. probe_id never changes, so this does not disqualify
-- HOT updates on the ingest hot path.
CREATE INDEX traceroute_current_probe_idx ON traceroute_current (probe_id);

-- Unindexed FK child column: session revocation / user cascade deletes.
CREATE INDEX sessions_user_idx ON sessions (user_id);

-- outage_events: cleanupSeries closes open probe_failing events by
-- probe_id (the existing partial unique index on the same predicate
-- leads with agent_id), and ListOutages' recently-closed branch
-- range-scans closed_at instead of walking the forever-retained
-- opened_at history every 30 s poll.
CREATE INDEX outage_events_failing_probe_idx ON outage_events (probe_id)
    WHERE kind = 'probe_failing' AND closed_at IS NULL;
CREATE INDEX outage_events_closed_idx ON outage_events (closed_at DESC)
    WHERE closed_at IS NOT NULL;

-- series_state last: the hottest write target (every ingest push upserts
-- it). Same probe_id rationale as traceroute_current; the partial
-- open_event_id index turns the 30 s sweep's orphan-repair join from a
-- full seq scan into a tiny index scan (only outage transitions write
-- the column — the rare non-HOT update this index costs).
CREATE INDEX series_state_probe_idx ON series_state (probe_id);
CREATE INDEX series_state_open_event_idx ON series_state (open_event_id)
    WHERE open_event_id IS NOT NULL;
