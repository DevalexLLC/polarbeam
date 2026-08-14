-- Per-site-pair threshold overrides. One row covers BOTH directions of a
-- pair: the key is the unordered site pair, canonicalized by uuid order
-- (site_a_id < site_b_id — bytewise, stable, invisible to users; the API
-- accepts either name order and sorts names only for display).
--
-- Each metric column is nullable: NULL means "inherit the global
-- dashboard_settings value for this field". Per-field CHECKs mirror the
-- global row's rules but only fire when the field is set; cross-field
-- CHECKs only when both sides are set. Consistency of the EFFECTIVE tuple
-- (override merged over global) is enforced in httpapi at write time — a
-- CHECK cannot see across tables. As with dashboard_settings, hitting a
-- CHECK from the API means a handler bug, and it should be loud.
--
-- ON DELETE CASCADE is deliberate: overrides are dashboard presentation
-- config and die with their site. DeleteSite's blocking-reference counts
-- must NOT include this table.
CREATE TABLE path_thresholds (
    site_a_id       uuid NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    site_b_id       uuid NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    latency_warn_us bigint,
    latency_crit_us bigint,
    loss_warn_pct   double precision,
    loss_crit_pct   double precision,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    updated_by      text NOT NULL DEFAULT '',
    PRIMARY KEY (site_a_id, site_b_id),
    CHECK (site_a_id < site_b_id),
    -- An all-NULL row would be a no-op that shadows "no override" in every
    -- listing; clearing an override is DELETE, not a row of NULLs.
    CHECK (num_nonnulls(latency_warn_us, latency_crit_us, loss_warn_pct, loss_crit_pct) > 0),
    CHECK (latency_warn_us IS NULL OR latency_warn_us > 0),
    CHECK (latency_crit_us IS NULL OR latency_crit_us > 0),
    CHECK (latency_warn_us IS NULL OR latency_crit_us IS NULL OR latency_crit_us > latency_warn_us),
    CHECK (loss_warn_pct IS NULL OR (loss_warn_pct >= 0 AND loss_warn_pct <= 100)),
    CHECK (loss_crit_pct IS NULL OR (loss_crit_pct > 0 AND loss_crit_pct <= 100)),
    CHECK (loss_warn_pct IS NULL OR loss_crit_pct IS NULL OR loss_crit_pct > loss_warn_pct)
);
-- The PK prefix covers site_a_id lookups; this serves the FK cascade scan
-- (and lookups) on the b side.
CREATE INDEX path_thresholds_b_idx ON path_thresholds (site_b_id);
