-- Login audit events: one row per successful dashboard sign-in, local or
-- SSO, kept forever.
--
-- user_id is deliberately NOT a foreign key: this audit log must outlive
-- its subject (docs/install.md documents DELETE FROM users as the cleanup
-- path), and the stable id is what keeps unique-user counts exact after a
-- deletion — an IdP-driven rename can put two usernames on one identity
-- within a month, so a username fallback would double-count. username,
-- role, and auth_source are snapshots taken at sign-in time; the latest
-- snapshot is what the account list shows for a deleted identity.
CREATE TABLE login_events (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL,
    username    text NOT NULL,
    role        text NOT NULL CHECK (role IN ('admin', 'viewer')),
    auth_source text NOT NULL CHECK (auth_source IN ('local', 'oidc')),
    occurred_at timestamptz NOT NULL DEFAULT now()
);

-- Per-user count(*) / max(occurred_at) for the admin account list.
CREATE INDEX login_events_user_idx ON login_events (user_id, occurred_at DESC);

-- 12-month window scan for the monthly activity chart.
CREATE INDEX login_events_time_idx ON login_events (occurred_at);
