-- Refresh + retention policy for probe_results_health_30m (0009). Policy
-- calls are plain functions and run fine inside this file's transaction;
-- if_not_exists keeps re-runs convergent.
--
-- Offset ordering is load-bearing, same reasoning as 0004:
--   start_offset (8d) > agent spool max_age (7d) — late spool replay still
--                       lands in a refreshable region;
--   start_offset (8d) < raw drop_after (14d) — refresh never reads a
--                       dropped region;
--   drop_after (14d)  > start_offset (8d) — retention never drops buckets
--                       the refresh policy still maintains, so the two jobs
--                       cannot churn against each other.
-- end_offset/schedule are one bucket: the health endpoints only ever query
-- the last 24 h, so the live tail (materialized_only = false) stays ≤1 h of
-- raw per query and each run only touches invalidated buckets.
-- Retention matches raw (14d): the strips query 24 h, so anything past the
-- 8 d refresh window is already slack; matching raw keeps this view and the
-- raw-backed bucket drill-down (AgentBucketFailures) covering the same
-- horizon.

SELECT add_continuous_aggregate_policy('probe_results_health_30m',
    start_offset      => interval '8 days',
    end_offset        => interval '30 minutes',
    schedule_interval => interval '30 minutes',
    if_not_exists     => true);

SELECT add_retention_policy('probe_results_health_30m', drop_after => interval '14 days', if_not_exists => true);
