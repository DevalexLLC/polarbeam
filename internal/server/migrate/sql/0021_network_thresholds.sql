-- Per-network threshold defaults: an overlay between the global
-- dashboard_settings singleton and the per-site-pair overrides.
--
-- A tenant's plane may have a different idea of "normal" than the ISP
-- management plane (a satellite backhaul versus metro fibre), and a
-- network_admin cannot write the global row — so without this layer a
-- tenant could only express itself pair by pair.
--
-- Every metric is nullable with the same NULL = inherit semantics as
-- path_thresholds, and the per-field/cross-field CHECKs mirror 0007's:
-- they fire only when the field (or both sides of a pair) is set.
-- Consistency of the EFFECTIVE tuple across layers is enforced in httpapi
-- at write time — a CHECK cannot see across tables — so hitting one of
-- these from the API means a handler bug and should be loud.
CREATE TABLE network_thresholds (
    network_id      uuid PRIMARY KEY REFERENCES networks (id) ON DELETE CASCADE,
    latency_warn_us bigint,
    latency_crit_us bigint,
    loss_warn_pct   double precision,
    loss_crit_pct   double precision,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    updated_by      text NOT NULL DEFAULT '',
    -- An all-NULL row is a no-op that shadows "no overlay" in every
    -- listing; clearing the overlay is DELETE, not a row of NULLs.
    CHECK (num_nonnulls(latency_warn_us, latency_crit_us, loss_warn_pct, loss_crit_pct) > 0),
    CHECK (latency_warn_us IS NULL OR latency_warn_us > 0),
    CHECK (latency_crit_us IS NULL OR latency_crit_us > 0),
    CHECK (latency_warn_us IS NULL OR latency_crit_us IS NULL OR latency_crit_us > latency_warn_us),
    CHECK (loss_warn_pct IS NULL OR (loss_warn_pct >= 0 AND loss_warn_pct <= 100)),
    CHECK (loss_crit_pct IS NULL OR (loss_crit_pct > 0 AND loss_crit_pct <= 100)),
    CHECK (loss_warn_pct IS NULL OR loss_crit_pct IS NULL OR loss_crit_pct > loss_warn_pct)
);
