-- probe_degraded outage events: per-series hysteresis over OK results that
-- breach the critical latency/loss thresholds (graded at ingest), alongside
-- the existing probe_failing hard-failure hysteresis. One open event per
-- series across both kinds — down supersedes degraded.

-- The 0001 kind CHECK was declared inline on the column, so it carries the
-- Postgres auto-generated name <table>_<column>_check.
ALTER TABLE outage_events DROP CONSTRAINT outage_events_kind_check;
ALTER TABLE outage_events ADD CONSTRAINT outage_events_kind_check
    CHECK (kind IN ('probe_failing', 'agent_offline', 'probe_degraded'));

-- Mirror of outage_events_probe_open_uidx for the new kind: openEvent's
-- ON CONFLICT inference needs a partial unique index whose predicate the
-- insert's conflict clause matches literally.
CREATE UNIQUE INDEX outage_events_degraded_open_uidx ON outage_events (agent_id, probe_id)
    WHERE kind = 'probe_degraded' AND closed_at IS NULL;

-- cleanupSeries and the orphan sweep now close BOTH probe kinds by probe_id.
-- A kind IN (...) predicate does not imply 0011's kind = 'probe_failing'
-- partial index predicate, so those queries could no longer use it; this
-- index's predicate matches their new predicate verbatim, and the old
-- single-kind index is fully superseded.
CREATE INDEX outage_events_probe_open_kind_idx ON outage_events (probe_id)
    WHERE kind IN ('probe_failing', 'probe_degraded') AND closed_at IS NULL;
DROP INDEX outage_events_failing_probe_idx;

-- Degraded (OK-but-breaching) and clean (OK-and-not-breaching) streak
-- counters beside the existing fail/OK pair. consec_oks keeps counting every
-- OK (it closes probe_failing); the clean tail is a separate streak because
-- it is not derivable from the other two. ADD COLUMN with a constant default
-- is catalog-only (no table rewrite), and series_state is written by every
-- ingest transaction, so this ALTER goes last to keep its ACCESS EXCLUSIVE
-- lock at the end of the migration (same rationale as 0011's ordering).
ALTER TABLE series_state
    ADD COLUMN consec_degraded   int NOT NULL DEFAULT 0,
    ADD COLUMN consec_clean      int NOT NULL DEFAULT 0,
    ADD COLUMN first_degraded_at timestamptz,
    ADD COLUMN first_clean_at    timestamptz;
