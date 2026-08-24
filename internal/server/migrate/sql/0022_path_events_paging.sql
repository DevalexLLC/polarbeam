-- Stable newest-first pagination adds id as the deterministic tie breaker.
-- This composite index subsumes the original time-only index.
CREATE INDEX path_events_time_id_idx ON path_events (time DESC, id DESC);
DROP INDEX path_events_time_idx;
