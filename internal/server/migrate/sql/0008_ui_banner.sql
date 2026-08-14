-- Admin-configurable UI banner: one shared text rendered in slim bands at
-- the top and bottom of every dashboard screen, sign-in included. Same
-- always-true-bool single-row pattern as dashboard_settings/oidc_settings.
-- The CHECKs mirror httpapi validation — hitting one from the API means a
-- handler bug, and it should be loud.
CREATE TABLE banner_settings (
    id         boolean PRIMARY KEY DEFAULT true CHECK (id),
    enabled    boolean NOT NULL DEFAULT false,
    text       text    NOT NULL DEFAULT '',
    updated_at timestamptz NOT NULL DEFAULT now(),
    updated_by text    NOT NULL DEFAULT '',
    CHECK (char_length(text) <= 300),
    -- Text may be staged while disabled, but enabling an empty banner is a
    -- mistake, not a policy.
    CHECK (NOT enabled OR text <> '')
);
-- Seeded here so GET never needs a missing-row branch and UPDATE always hits.
INSERT INTO banner_settings DEFAULT VALUES;
