-- Give per-site-pair threshold overrides a network dimension.
--
-- A site pair can legitimately span planes: one site may host both an ISP
-- management agent and a tenant agent, so "the override for site A ↔ site
-- B" was ambiguous the moment 0017 landed. Rather than infer a plane for
-- existing rows, add the dimension and let NULL keep meaning what today's
-- rows already mean.
--
--   network_id IS NULL  — applies to every plane (today's behavior,
--                         preserved bit-for-bit; global-admin-only to write)
--   network_id SET      — applies to that plane only; a network_admin
--                         scoped to it may write it
--
-- Resolution is per metric, most specific wins:
--   pair+network → pair (NULL plane) → network default → global default
-- and is mirrored in internal/server/thresholds, httpapi, and the SPA.
--
-- ON DELETE CASCADE matches the site FKs above it: an override is dashboard
-- presentation config and dies with the plane it describes. DeleteNetwork's
-- blocking-reference counts must NOT include this table.
ALTER TABLE path_thresholds ADD COLUMN network_id uuid REFERENCES networks (id) ON DELETE CASCADE;

-- The pair PK becomes a three-column uniqueness rule. NULLS NOT DISTINCT
-- (PG15+; the pinned timescaledb-ha:pg16-all image qualifies) is what makes
-- the all-planes row unique too — under the default NULLS DISTINCT a pair
-- could accumulate unlimited NULL-plane rows and the upsert's ON CONFLICT
-- would never fire.
--
-- Dropping the PK drops its index, which served both the site_a_id lookups
-- and NOT NULL. The replacement UNIQUE index leads with the same two
-- columns, so PathThresholdPairs' site_a_id arm keeps its index scan;
-- path_thresholds_b_idx still serves the b-side arm and the FK cascade.
-- NOT NULL is restated explicitly because the PK was carrying it.
ALTER TABLE path_thresholds DROP CONSTRAINT path_thresholds_pkey;
ALTER TABLE path_thresholds ALTER COLUMN site_a_id SET NOT NULL;
ALTER TABLE path_thresholds ALTER COLUMN site_b_id SET NOT NULL;
ALTER TABLE path_thresholds ADD CONSTRAINT path_thresholds_pair_network_key
    UNIQUE NULLS NOT DISTINCT (site_a_id, site_b_id, network_id);
