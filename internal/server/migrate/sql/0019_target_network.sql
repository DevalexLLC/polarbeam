-- Target ownership: external targets may belong to a network (a tenant's
-- plane) instead of the operator. NULL means global — operator-owned,
-- readable by every scope, writable only by a global admin. A set
-- network_id makes the row tenant-owned: visible to that plane's users and
-- writable by its network_admin.
--
-- Agent-kind targets are deliberately excluded. They are synthesized one
-- per agent and their plane is already stated by agents.network_id; a
-- second, independently writable copy could drift from it. The CHECK below
-- makes that non-representable rather than a convention.
--
-- ON DELETE RESTRICT, not CASCADE: a target is probe workload, not
-- presentation config, and silently deleting a tenant's targets with its
-- network would take the probe_configs referencing them along by surprise.
-- DeleteNetwork counts these rows among its blockers so the refusal names
-- them instead of surfacing an opaque FK violation.
--
-- Names stay globally unique (0001's targets.name UNIQUE is untouched): a
-- tenant claiming a name that already exists gets a 409. Per-plane name
-- scoping would ripple into target identity in the config snapshot and the
-- wire format for no real gain.
ALTER TABLE targets ADD COLUMN network_id uuid REFERENCES networks (id) ON DELETE RESTRICT;
ALTER TABLE targets ADD CONSTRAINT targets_network_external_check
    CHECK (network_id IS NULL OR kind = 'external');

-- Serves the DeleteNetwork blocker count and the scoped target listing.
CREATE INDEX targets_network_idx ON targets (network_id) WHERE network_id IS NOT NULL;
