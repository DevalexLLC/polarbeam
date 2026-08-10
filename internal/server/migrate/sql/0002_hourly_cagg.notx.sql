-- Hourly continuous aggregate over probe_results.
--
-- notx file: exactly ONE statement, idempotent (see migrate.go package doc).
--
-- Group keys (bucket, agent_id, target_id, probe_type, latency_source)
-- serve the existing SiteEndpoints agent_id/target_id = ANY(...) filters
-- with no joins, same as the raw queries. latency_source partitions each
-- bucket by the row's timing family (which column the COALESCE ladder
-- resolved), so a dashboard series can select RTT without mixing in
-- TCP/application timings, and every timing statistic includes successful
-- probes only — a fast failure must never read as low latency.
--
-- Every measure is a sum/count/min/max — never avg() — so the daily cagg
-- in 0003 rolls up exactly (avg of avgs would be wrong once bucket sample
-- counts differ). Averages are computed at query time as sum/count.
--
-- The COALESCE latency ladder mirrors latencyExpr in
-- internal/server/store/dashboard.go and must change in lockstep with it;
-- this copy is frozen once the migration ships (a ladder change later needs
-- a new cagg version). Traceroute rows carry NULL timings, so they land in
-- the '' family and fall out of every latency aggregate but still count
-- into samples/sent/received — identical to the raw-query semantics.
--
-- materialized_only = false: queries union the not-yet-refreshed tail live
-- from raw, so charts are correct before the first policy refresh.

CREATE MATERIALIZED VIEW IF NOT EXISTS probe_results_hourly
WITH (timescaledb.continuous, timescaledb.materialized_only = false) AS
SELECT
    time_bucket('1 hour', time) AS bucket,
    agent_id,
    target_id,
    probe_type,
    CASE
        WHEN rtt_avg_us       IS NOT NULL THEN 'rtt'
        WHEN tcp_connect_us   IS NOT NULL THEN 'tcp_connect'
        WHEN tls_handshake_us IS NOT NULL THEN 'tls_handshake'
        WHEN ttfb_us          IS NOT NULL THEN 'ttfb'
        WHEN total_us         IS NOT NULL THEN 'total'
        ELSE ''
    END AS latency_source,
    count(*)                            AS samples,
    count(*) FILTER (WHERE status = 1)  AS ok_samples,
    sum(sent)                           AS sent,
    sum(received)                       AS received,
    max(time) FILTER (WHERE status = 1) AS last_ok_at,
    min(CASE WHEN status = 1 THEN COALESCE(rtt_avg_us, tcp_connect_us, tls_handshake_us, ttfb_us, total_us) END) AS lat_min_us,
    max(CASE WHEN status = 1 THEN COALESCE(rtt_avg_us, tcp_connect_us, tls_handshake_us, ttfb_us, total_us) END) AS lat_max_us,
    sum(CASE WHEN status = 1 THEN COALESCE(rtt_avg_us, tcp_connect_us, tls_handshake_us, ttfb_us, total_us)::bigint END) AS lat_sum_us,
    count(CASE WHEN status = 1 THEN COALESCE(rtt_avg_us, tcp_connect_us, tls_handshake_us, ttfb_us, total_us) END) AS lat_count,
    percentile_agg((CASE WHEN status = 1 THEN COALESCE(rtt_avg_us, tcp_connect_us, tls_handshake_us, ttfb_us, total_us) END)::double precision) AS lat_pctl,
    sum(jitter_us::bigint) FILTER (WHERE status = 1)        AS jitter_sum_us,
    count(jitter_us) FILTER (WHERE status = 1)              AS jitter_count,
    max(jitter_us) FILTER (WHERE status = 1)                AS jitter_max_us,
    sum(tcp_connect_us::bigint) FILTER (WHERE status = 1)   AS tcp_sum_us,
    count(tcp_connect_us) FILTER (WHERE status = 1)         AS tcp_count,
    sum(tls_handshake_us::bigint) FILTER (WHERE status = 1) AS tls_sum_us,
    count(tls_handshake_us) FILTER (WHERE status = 1)       AS tls_count
FROM probe_results
GROUP BY bucket, agent_id, target_id, probe_type, latency_source
WITH NO DATA;
