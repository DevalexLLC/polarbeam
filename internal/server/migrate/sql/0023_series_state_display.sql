-- Matrix display columns on series_state.
--
-- The matrix's "latest per series" query used to DISTINCT ON over the raw
-- hypertable's last 10 minutes — a scan of the whole fleet's recent rows on
-- every 30s dashboard poll, scaling with total result rate rather than
-- series count. series_state already holds last_status/last_time per
-- series, maintained by the ingest transaction; these columns add the rest
-- of what a matrix cell renders, so the query becomes an O(series) read.
--
-- Values are written by the same ingest fold that maintains last_status
-- (internal/server/outage), mirroring the raw query's semantics: the
-- COALESCE latency ladder of the newest row, failures included. NULL until
-- a series' first result after this upgrade (one probe interval) and
-- whenever that result measured nothing.
ALTER TABLE series_state
    ADD COLUMN last_loss_pct       real,
    ADD COLUMN last_latency_us     bigint,
    ADD COLUMN last_latency_source text;
