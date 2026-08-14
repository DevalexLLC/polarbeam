-- 30-minute health continuous aggregate over probe_results.
--
-- notx file: exactly ONE statement, idempotent (see migrate.go package doc).
--
-- Purpose-built for the agent health strips (GET /api/v1/agents/health and
-- /api/v1/agents/{id}/health), which the SPA polls every 30 s and which
-- previously scanned 24 h of raw rows per call. Group keys
-- (bucket, agent_id, probe_id, probe_type) serve both readers:
-- AgentHealthSeries folds over agent_id and filters probe_type at read time
-- (the fleet strip excludes traceroute, whose run-accounting rows would
-- poison a success ratio), while AgentProbeHealth folds per probe_id with
-- traceroute included. Traceroute rows are therefore aggregated here like
-- any other type — exclusion is the reader's decision, not the view's.
--
-- status = 1 is the only success; everything else — UNSUPPORTED included —
-- counts as a failure, matching the raw queries this view replaces. Both
-- measures are plain counts, so re-bucketing to any multiple of 30 minutes
-- is an exact sum() rollup.
--
-- materialized_only = false: queries union the not-yet-refreshed tail live
-- from raw, so the strip's current half hour is fresh between policy runs
-- and correct before the first refresh.

CREATE MATERIALIZED VIEW IF NOT EXISTS probe_results_health_30m
WITH (timescaledb.continuous, timescaledb.materialized_only = false) AS
SELECT
    time_bucket('30 minutes', time) AS bucket,
    agent_id,
    probe_id,
    probe_type,
    count(*)                           AS samples,
    count(*) FILTER (WHERE status = 1) AS ok_samples
FROM probe_results
GROUP BY bucket, agent_id, probe_id, probe_type
WITH NO DATA;
