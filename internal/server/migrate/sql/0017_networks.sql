-- Networks: named connectivity planes. Two agents at one site may sit on
-- mutually unreachable networks (an ISP's management plane vs a tenant
-- plane), so every scoping object — agent, join token, mesh group, direct
-- probe — belongs to exactly one network, and expansion never pairs agents
-- across the boundary. A network asserts reachability and (later) ownership;
-- it deliberately models neither subnets nor VLANs.

CREATE TABLE networks (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name         text NOT NULL UNIQUE,
    display_name text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- Seeded so every pre-networks object lands here and the new expansion
-- predicates are no-ops on upgrade (probe specs and config hashes must not
-- move). Same seed-in-migration pattern as 0001's dashboard_settings.
INSERT INTO networks (name, display_name) VALUES ('default', 'Default');

-- ADD COLUMN + backfill + SET NOT NULL inside this one transaction: these
-- tables are fleet-sized, so the rewrite/lock cost is nil. Deliberately no
-- column DEFAULT: enrollment copies the agent's network from its token and
-- admin writers resolve 'default' explicitly, so a code path that forgets
-- to assign one fails loudly on NOT NULL instead of silently landing on
-- default.

ALTER TABLE agents ADD COLUMN network_id uuid REFERENCES networks(id);
UPDATE agents SET network_id = (SELECT id FROM networks WHERE name = 'default');
ALTER TABLE agents ALTER COLUMN network_id SET NOT NULL;
CREATE INDEX agents_network_idx ON agents (network_id);

ALTER TABLE join_tokens ADD COLUMN network_id uuid REFERENCES networks(id);
UPDATE join_tokens SET network_id = (SELECT id FROM networks WHERE name = 'default');
ALTER TABLE join_tokens ALTER COLUMN network_id SET NOT NULL;

ALTER TABLE mesh_groups ADD COLUMN network_id uuid REFERENCES networks(id);
UPDATE mesh_groups SET network_id = (SELECT id FROM networks WHERE name = 'default');
ALTER TABLE mesh_groups ALTER COLUMN network_id SET NOT NULL;

-- Direct probe rows carry their own network (the probe runs only on that
-- network's agents at the site); mesh template rows inherit the mesh's
-- network and must keep network_id NULL so the mesh's plane is stated in
-- exactly one place and can never drift.
ALTER TABLE probe_configs ADD COLUMN network_id uuid REFERENCES networks(id);
UPDATE probe_configs SET network_id = (SELECT id FROM networks WHERE name = 'default')
 WHERE mesh_id IS NULL;

-- The 0001 direct/mesh XOR CHECK was a table-level unnamed constraint, so it
-- carries the Postgres auto-generated name <table>_check (cf. 0013 dropping
-- the auto-named outage_events_kind_check). Replace it with one that also
-- pins network_id to the direct half.
ALTER TABLE probe_configs DROP CONSTRAINT probe_configs_check;
ALTER TABLE probe_configs ADD CONSTRAINT probe_configs_check CHECK (
    (mesh_id IS NOT NULL AND site_id IS NULL AND target_id IS NULL AND network_id IS NULL)
 OR (mesh_id IS NULL AND site_id IS NOT NULL AND target_id IS NOT NULL AND network_id IS NOT NULL));
