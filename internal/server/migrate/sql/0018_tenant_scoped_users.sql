-- Network-scoped tenant users: two new roles (network_admin,
-- network_viewer) whose dashboard visibility is limited to an explicit set
-- of networks. Scope lives in user_networks, one row per (user, network) —
-- a user may span several planes (an MSP tenant with two networks). Global
-- roles (admin, viewer) never have user_networks rows; scoped roles must
-- have at least one to see anything (an empty set fails closed to "sees
-- nothing", it never widens to "sees everything").
--
-- The role CHECKs were column constraints in 0001/0006, so they carry the
-- Postgres auto-generated <table>_<column>_check names (cf. 0017 dropping
-- the auto-named probe_configs_check).

ALTER TABLE users DROP CONSTRAINT users_role_check;
ALTER TABLE users ADD CONSTRAINT users_role_check
    CHECK (role IN ('admin', 'viewer', 'network_admin', 'network_viewer'));

ALTER TABLE login_events DROP CONSTRAINT login_events_role_check;
ALTER TABLE login_events ADD CONSTRAINT login_events_role_check
    CHECK (role IN ('admin', 'viewer', 'network_admin', 'network_viewer'));

-- ON DELETE CASCADE both ways: deleting a user drops its scope, and
-- deleting a network silently un-scopes users from it — user scope must
-- never block operator topology changes, so DeleteNetwork deliberately does
-- NOT count these rows among its delete blockers.
CREATE TABLE user_networks (
    user_id    uuid NOT NULL REFERENCES users (id)    ON DELETE CASCADE,
    network_id uuid NOT NULL REFERENCES networks (id) ON DELETE CASCADE,
    PRIMARY KEY (user_id, network_id)
);
CREATE INDEX user_networks_network_idx ON user_networks (network_id);

-- OIDC claim → scoped-role mapping, evaluated after admin_values (which
-- always wins): a JSON array of rules
--   [{"value": "<claim value>", "role": "network_admin"|"network_viewer",
--     "networks": ["<network name>", ...]}, ...]
-- The strongest matched role wins (network_admin > network_viewer) and the
-- networks of every rule granting it are unioned. unmatched_role decides
-- what an authenticated user matching nothing becomes: 'viewer' preserves
-- the pre-tenancy behavior (every SSO user is at least a global viewer);
-- 'deny' refuses the login — the posture multi-tenant installs need, since
-- a global viewer sees every plane.
ALTER TABLE oidc_settings ADD COLUMN role_rules jsonb NOT NULL DEFAULT '[]';
ALTER TABLE oidc_settings ADD COLUMN unmatched_role text NOT NULL DEFAULT 'viewer'
    CONSTRAINT oidc_settings_unmatched_role_check
    CHECK (unmatched_role IN ('viewer', 'deny'));
