-- Refresh + retention policies. Policy calls are plain functions and run
-- fine inside this file's transaction; if_not_exists keeps re-runs
-- convergent should this file ever race a partial apply.
--
-- MUST come after both caggs exist (0002/0003): retention on raw is only
-- safe once a refresh policy is in place to roll the data up first.
--
-- Offset ordering is load-bearing:
--   hourly start_offset (8d)  > agent spool max_age (7d) — late spool
--                               replay still lands in a refreshable region;
--   hourly start_offset (8d)  < raw drop_after (14d) — refresh never reads
--                               a dropped region;
--   daily  start_offset (10d) > hourly's late-arrival window.
-- end_offset trades freshness for refresh cost; the un-materialized tail is
-- served live thanks to materialized_only = false on both caggs.

SELECT add_continuous_aggregate_policy('probe_results_hourly',
    start_offset      => interval '8 days',
    end_offset        => interval '1 hour',
    schedule_interval => interval '1 hour',
    if_not_exists     => true);

SELECT add_continuous_aggregate_policy('probe_results_daily',
    start_offset      => interval '10 days',
    end_offset        => interval '1 day',
    schedule_interval => interval '1 day',
    if_not_exists     => true);

-- Retention per docs/architecture.md: raw 14d, hourly 100d, daily 400d.
SELECT add_retention_policy('probe_results',        drop_after => interval '14 days',  if_not_exists => true);
SELECT add_retention_policy('probe_results_hourly', drop_after => interval '100 days', if_not_exists => true);
SELECT add_retention_policy('probe_results_daily',  drop_after => interval '400 days', if_not_exists => true);
