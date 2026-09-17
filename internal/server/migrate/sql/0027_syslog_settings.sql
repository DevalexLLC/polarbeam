-- Syslog forwarding destination, edited from Settings -> Log forwarding
-- and applied without a restart: the single-row settings pattern of
-- banner_settings/oidc_settings. The CHECKs mirror the API validation
-- (internal/server/syslogfwd Config.Problems) — hitting one from the API
-- is a handler bug and should be loud. The client private key is
-- write-only through the API, like oidc_settings.client_secret.
CREATE TABLE syslog_settings (
    id                  boolean PRIMARY KEY DEFAULT true CHECK (id),
    enabled             boolean     NOT NULL DEFAULT false,
    transport           text        NOT NULL DEFAULT 'tls' CHECK (transport IN ('udp', 'tcp', 'tls')),
    host                text        NOT NULL DEFAULT '',
    port                integer     NOT NULL DEFAULT 6514 CHECK (port BETWEEN 1 AND 65535),
    framing             text        NOT NULL DEFAULT 'octet-counted' CHECK (framing IN ('octet-counted', 'non-transparent')),
    facility            smallint    NOT NULL DEFAULT 16 CHECK (facility BETWEEN 0 AND 23),
    hostname            text        NOT NULL DEFAULT '',
    content             text        NOT NULL DEFAULT 'all' CHECK (content IN ('all', 'audit')),
    min_level           text        NOT NULL DEFAULT 'info' CHECK (min_level IN ('debug', 'info', 'warn', 'error')),
    on_failure          text        NOT NULL DEFAULT 'warn' CHECK (on_failure IN ('warn', 'halt')),
    -- Milliseconds, the convention probe_configs.interval_ms set.
    failure_timeout_ms  bigint      NOT NULL DEFAULT 300000 CHECK (failure_timeout_ms >= 60000),
    tls_ca_pem          text        NOT NULL DEFAULT '',
    tls_client_cert_pem text        NOT NULL DEFAULT '',
    tls_client_key_pem  text        NOT NULL DEFAULT '',
    tls_server_name     text        NOT NULL DEFAULT '',
    updated_at          timestamptz NOT NULL DEFAULT now(),
    updated_by          text        NOT NULL DEFAULT '',
    -- Enabling without a destination is a mistake, not a policy.
    CHECK (NOT enabled OR host <> ''),
    CHECK (NOT (transport = 'tls' AND framing = 'non-transparent'))
);
-- Seeded so GET never needs a missing-row branch and UPDATE always hits.
INSERT INTO syslog_settings DEFAULT VALUES;
