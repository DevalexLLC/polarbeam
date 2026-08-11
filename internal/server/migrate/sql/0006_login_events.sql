-- Login audit events: one row per successful dashboard sign-in, local or
-- SSO, kept forever.
--
-- user_id is deliberately NOT a foreign key: this audit log must outlive
-- its subject (docs/install.md documents DELETE FROM users as the cleanup
-- path), and the stable id is what keeps unique-user counts exact after a
-- deletion. username, role, and auth_source are snapshots taken at
-- sign-in time; the latest snapshot is what the account list shows for a
-- deleted identity.
--
-- identity is the stable per-person key for unique-user counts, where
-- user_id alone is wrong in both directions: an IdP-driven rename puts two
-- usernames on one account (username would double-count), and a deleted
-- SSO account gets JIT-reprovisioned under a fresh uuid on the next login
-- (user_id would double-count). SSO identities key on issuer + subject,
-- which survive both; local identities key on username (local accounts
-- have no rename path).
CREATE TABLE login_events (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL,
    identity    text NOT NULL,
    username    text NOT NULL,
    role        text NOT NULL CHECK (role IN ('admin', 'viewer')),
    auth_source text NOT NULL CHECK (auth_source IN ('local', 'oidc')),
    occurred_at timestamptz NOT NULL DEFAULT now()
);

-- Per-user count(*) / max(occurred_at) for the admin account list.
CREATE INDEX login_events_user_idx ON login_events (user_id, occurred_at DESC);

-- 12-month window scan for the monthly activity chart.
CREATE INDEX login_events_time_idx ON login_events (occurred_at);
