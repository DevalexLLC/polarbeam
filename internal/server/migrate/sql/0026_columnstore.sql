-- Columnstore (compression) for the raw hypertable and the two hourly
-- continuous aggregates. Policy calls are plain functions
-- and run inside this file's transaction (same shape as 0004/0010/0016);
-- if_not_exists keeps re-runs convergent. Settings apply to chunks
-- compressed from now on; each policy's first run compresses every
-- already-eligible chunk in the background, one chunk per transaction.
--
-- Why: on a compressed chunk TimescaleDB drops the per-chunk B-trees
-- (two thirds of the raw hypertable's footprint) and stores every column
-- with a type-specific codec, so cold chunks shrink by an order of
-- magnitude and long-window reads scan columnar batches with segment
-- pruning instead of heap pages.
--
-- Offset ordering is load-bearing (extends the 0004/0010/0016 chain):
--   raw compress_after (8d) > agent spool max_age (7d)   — no spool replay
--                                     ever lands in a compressed chunk;
--   raw compress_after (8d) >= raw-based cagg start_offset (8d) — a refresh
--                                     never reads a compressed raw region;
--   raw compress_after (8d) > longest raw dashboard window (7d);
--   raw compress_after (8d) < raw drop_after (14d)       — days 8–14 are
--                                     read by nothing and compress for free;
--   hourly/stage_hourly compress_after (10d) > their start_offset (8d);
--   probe_results_health_30m is NOT compressed: 14d retention on its
--   10-day materialization chunks leaves at most ~4 useful days;
--   probe_results_daily / _stage_daily are NOT compressed: one row per
--   series per day means ~10 rows per segment in a 10-day chunk, and the
--   measured ratio (1.6x / 1.8x, uddsketch state serialized as text) did
--   not clear the 2x bar that justifies a policy on relations this small.
--
-- Raw segmentby must cover the unique dedupe index (agent_id, probe_id,
-- time) together with orderby — a compressed hypertable requires it.
-- target_id is functionally dependent on probe_id (a probe has exactly one
-- target), so it adds no segments but lets the pair queries prune batches
-- by (agent_id, target_id) should a compressed chunk ever be read.
-- Cagg segmentby/orderby are pinned to their group keys explicitly rather
-- than left to TimescaleDB's defaults: this file is frozen once shipped and
-- defaults may drift between extension versions.
--
-- The raw hypertable is the ingest write target and its ALTER takes a brief
-- exclusive lock, so it goes last (same live-upgrade ordering as 0011).

ALTER MATERIALIZED VIEW probe_results_hourly SET (
    timescaledb.enable_columnstore = true,
    timescaledb.segmentby = 'agent_id, target_id, probe_type, latency_source',
    timescaledb.orderby   = 'bucket DESC');
SELECT add_compression_policy('probe_results_hourly',
    compress_after    => interval '10 days',
    schedule_interval => interval '1 day',
    if_not_exists     => true);

ALTER MATERIALIZED VIEW probe_results_stage_hourly SET (
    timescaledb.enable_columnstore = true,
    timescaledb.segmentby = 'agent_id, target_id, probe_type',
    timescaledb.orderby   = 'bucket DESC');
SELECT add_compression_policy('probe_results_stage_hourly',
    compress_after    => interval '10 days',
    schedule_interval => interval '1 day',
    if_not_exists     => true);

ALTER TABLE probe_results SET (
    timescaledb.enable_columnstore = true,
    timescaledb.segmentby = 'agent_id, target_id, probe_id',
    timescaledb.orderby   = 'time DESC');
SELECT add_compression_policy('probe_results',
    compress_after    => interval '8 days',
    schedule_interval => interval '1 hour',
    if_not_exists     => true);
