-- Hourly stage-timing continuous aggregate over probe_results, for the
-- target detail page's stage breakdown chart (DNS / TCP connect / TLS
-- handshake / TTFB / total).
--
-- notx file: exactly ONE statement, idempotent (see migrate.go package doc).
--
-- A sibling of probe_results_hourly (0002), NOT a replacement: that cagg
-- shipped in v0.3.0 and is immutable, and recreating it could only
-- rematerialize from raw (14d retention), destroying long-window history.
-- This view deliberately repeats tcp/tls sums so stage queries read one
-- coherent view instead of stitching two caggs.
--
-- Group keys (bucket, agent_id, target_id, probe_type) serve the same
-- agent_id/target_id = ANY(...) filters as 0002 with no joins. There is no
-- latency_source partition: each stage is its own column, so per-stage
-- count()s already exclude rows that did not measure that stage (traceroute,
-- path_mtu, icmp carry NULL application timings and fall out naturally).
-- Every timing statistic includes successful probes only — a fast failure
-- must never read as low latency.
--
-- Sums/counts only, never avg(), so the daily rollup in 0015 is exact;
-- averages are computed at query time as sum/count.
--
-- On upgrade this cagg backfills only from surviving raw rows (≤14d), so
-- 30d+ stage charts fill forward over time; the dashboard renders the gap
-- as an empty state.
--
-- materialized_only = false: queries union the not-yet-refreshed tail live
-- from raw, so charts are correct before the first policy refresh.

CREATE MATERIALIZED VIEW IF NOT EXISTS probe_results_stage_hourly
WITH (timescaledb.continuous, timescaledb.materialized_only = false) AS
SELECT
    time_bucket('1 hour', time) AS bucket,
    agent_id,
    target_id,
    probe_type,
    count(*) FILTER (WHERE status = 1)                      AS ok_samples,
    sum(dns_us::bigint) FILTER (WHERE status = 1)           AS dns_sum_us,
    count(dns_us) FILTER (WHERE status = 1)                 AS dns_count,
    sum(tcp_connect_us::bigint) FILTER (WHERE status = 1)   AS tcp_sum_us,
    count(tcp_connect_us) FILTER (WHERE status = 1)         AS tcp_count,
    sum(tls_handshake_us::bigint) FILTER (WHERE status = 1) AS tls_sum_us,
    count(tls_handshake_us) FILTER (WHERE status = 1)       AS tls_count,
    sum(ttfb_us::bigint) FILTER (WHERE status = 1)          AS ttfb_sum_us,
    count(ttfb_us) FILTER (WHERE status = 1)                AS ttfb_count,
    sum(total_us::bigint) FILTER (WHERE status = 1)         AS total_sum_us,
    count(total_us) FILTER (WHERE status = 1)               AS total_count
FROM probe_results
GROUP BY bucket, agent_id, target_id, probe_type
WITH NO DATA;
