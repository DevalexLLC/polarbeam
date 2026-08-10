-- Daily continuous aggregate, hierarchical rollup of the hourly cagg.
--
-- notx file: exactly ONE statement, idempotent (see migrate.go package doc).
--
-- Sums and counts re-sum, min/max re-fold, and the UddSketch percentile
-- state rolls up losslessly via rollup() — that rollup-ability is why
-- percentile_agg was chosen over exact percentiles. latency_source stays a
-- group key so the hourly timing-family partition survives the rollup.

CREATE MATERIALIZED VIEW IF NOT EXISTS probe_results_daily
WITH (timescaledb.continuous, timescaledb.materialized_only = false) AS
SELECT
    time_bucket('1 day', bucket) AS bucket,
    agent_id,
    target_id,
    probe_type,
    latency_source,
    sum(samples)       AS samples,
    sum(ok_samples)    AS ok_samples,
    sum(sent)          AS sent,
    sum(received)      AS received,
    max(last_ok_at)    AS last_ok_at,
    min(lat_min_us)    AS lat_min_us,
    max(lat_max_us)    AS lat_max_us,
    sum(lat_sum_us)    AS lat_sum_us,
    sum(lat_count)     AS lat_count,
    rollup(lat_pctl)   AS lat_pctl,
    sum(jitter_sum_us) AS jitter_sum_us,
    sum(jitter_count)  AS jitter_count,
    max(jitter_max_us) AS jitter_max_us,
    sum(tcp_sum_us)    AS tcp_sum_us,
    sum(tcp_count)     AS tcp_count,
    sum(tls_sum_us)    AS tls_sum_us,
    sum(tls_count)     AS tls_count
FROM probe_results_hourly
GROUP BY 1, agent_id, target_id, probe_type, latency_source
WITH NO DATA;
