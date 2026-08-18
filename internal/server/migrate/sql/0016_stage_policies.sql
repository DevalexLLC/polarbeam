-- Refresh + retention policies and pair indexes for the stage caggs
-- (0014/0015). Policy calls are plain functions and run fine inside this
-- file's transaction; if_not_exists keeps re-runs convergent should this
-- file ever race a partial apply.
--
-- Offsets mirror 0004, and its ordering rationale is load-bearing here too:
--   hourly start_offset (8d)  > agent spool max_age (7d) — late spool
--                               replay still lands in a refreshable region;
--   hourly start_offset (8d)  < raw drop_after (14d) — refresh never reads
--                               a dropped region;
--   daily  start_offset (10d) > hourly's late-arrival window.
-- Retention matches 0004 (hourly 100d, daily 400d) so stage charts cover
-- exactly the same horizons as the latency charts.

SELECT add_continuous_aggregate_policy('probe_results_stage_hourly',
    start_offset      => interval '8 days',
    end_offset        => interval '1 hour',
    schedule_interval => interval '1 hour',
    if_not_exists     => true);

SELECT add_continuous_aggregate_policy('probe_results_stage_daily',
    start_offset      => interval '10 days',
    end_offset        => interval '1 day',
    schedule_interval => interval '1 day',
    if_not_exists     => true);

SELECT add_retention_policy('probe_results_stage_hourly', drop_after => interval '100 days', if_not_exists => true);
SELECT add_retention_policy('probe_results_stage_daily',  drop_after => interval '400 days', if_not_exists => true);

-- Stage queries filter both caggs on agent_id + target_id + bucket;
-- TimescaleDB's auto-created indexes are single-key (same rationale as
-- 0011). These land on the materialization hypertables, so the write cost
-- falls on cagg refresh, not raw ingest.
CREATE INDEX probe_results_stage_hourly_pair_idx
    ON probe_results_stage_hourly (agent_id, target_id, bucket DESC);
CREATE INDEX probe_results_stage_daily_pair_idx
    ON probe_results_stage_daily (agent_id, target_id, bucket DESC);

-- The target detail endpoints resolve sources and health inventories by
-- filtering series_state on target_id, which only the (agent_id, probe_id)
-- PK and probe_id indexes cannot serve — without this, every target-page
-- poll seq-scans series_state four times. target_id never changes after
-- insert, so ingest's status upserts stay HOT-eligible (same rationale as
-- series_state_probe_idx in 0011).
CREATE INDEX series_state_target_idx ON series_state (target_id);
