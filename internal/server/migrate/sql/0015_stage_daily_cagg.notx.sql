-- Daily stage-timing continuous aggregate, hierarchical rollup of 0014.
--
-- notx file: exactly ONE statement, idempotent (see migrate.go package doc).
--
-- Sums and counts re-sum exactly (the hourly cagg stores no averages), so
-- the daily averages computed at query time equal what raw would give.

CREATE MATERIALIZED VIEW IF NOT EXISTS probe_results_stage_daily
WITH (timescaledb.continuous, timescaledb.materialized_only = false) AS
SELECT
    time_bucket('1 day', bucket) AS bucket,
    agent_id,
    target_id,
    probe_type,
    sum(ok_samples)   AS ok_samples,
    sum(dns_sum_us)   AS dns_sum_us,
    sum(dns_count)    AS dns_count,
    sum(tcp_sum_us)   AS tcp_sum_us,
    sum(tcp_count)    AS tcp_count,
    sum(tls_sum_us)   AS tls_sum_us,
    sum(tls_count)    AS tls_count,
    sum(ttfb_sum_us)  AS ttfb_sum_us,
    sum(ttfb_count)   AS ttfb_count,
    sum(total_sum_us) AS total_sum_us,
    sum(total_count)  AS total_count
FROM probe_results_stage_hourly
GROUP BY 1, agent_id, target_id, probe_type
WITH NO DATA;
