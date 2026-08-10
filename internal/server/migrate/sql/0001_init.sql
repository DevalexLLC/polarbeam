-- PolarBEAM baseline schema: identity/enrollment, probe configuration,
-- the probe_results hypertable, dashboard users/sessions/settings, OIDC,
-- and outage/path state.

-- TimescaleDB Toolkit is a hard dependency — percentile_agg (UddSketch)
-- backs the p50/p95/p99 columns in the continuous aggregates (0002/0003).
-- It ships in the timescale/timescaledb-ha image; CREATE EXTENSION needs
-- superuser (compose's POSTGRES_USER is superuser in that image). serve
-- preflight re-checks the extension so a hand-built DB fails loud, not at
-- query time.
CREATE EXTENSION IF NOT EXISTS timescaledb_toolkit;

-- ---- identity and enrollment ----

CREATE TABLE sites (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name         text NOT NULL UNIQUE,
    display_name text NOT NULL DEFAULT '',
    location     text NOT NULL DEFAULT '',
    -- Map position, set via `polarbeam-server site set` (never at
    -- enrollment). Both set or both NULL; NULL = not placed on the map.
    latitude     double precision,
    longitude    double precision,
    created_at   timestamptz NOT NULL DEFAULT now(),
    CHECK ((latitude IS NULL) = (longitude IS NULL)),
    CHECK (latitude IS NULL OR latitude BETWEEN -90 AND 90),
    CHECK (longitude IS NULL OR longitude BETWEEN -180 AND 180)
);

CREATE TABLE agents (
    id                  uuid PRIMARY KEY,
    site_id             uuid NOT NULL REFERENCES sites(id),
    hostname            text NOT NULL,
    -- Address peers should use to probe this agent (NAT-safe; defaults to
    -- the source address observed at enrollment).
    probe_address       text NOT NULL DEFAULT '',
    version             text NOT NULL DEFAULT '',
    last_seen_at        timestamptz,
    current_config_hash text NOT NULL DEFAULT '',
    -- Running total of spooled results the agent reported losing to spool
    -- bounds enforcement. Agents report a lifetime dropped_total; the
    -- server folds delta = dropped_total - dropped_last_total inside the
    -- ingest transaction (a backwards total means the agent's spool state
    -- was wiped, in which case the whole new total counts and the baseline
    -- resets), making dropped_results exact under retries. Agents that
    -- only send the legacy dropped_since_last_push delta take the
    -- delta-in-transaction path, which can overcount on a lost response
    -- after a successful commit — an operator alarm signal either way.
    dropped_results     bigint NOT NULL DEFAULT 0,
    -- NULL means "no total-bearing report seen yet" — deliberately NOT 0,
    -- so the first total-bearing report from an agent with acknowledged
    -- legacy-delta drops counts only the unacknowledged portion.
    dropped_last_total  bigint,
    last_dropped_at     timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX agents_site_idx ON agents (site_id);

CREATE TABLE join_tokens (
    id            uuid PRIMARY KEY,
    -- sha256 of the secret half of "<id>.<secret>"; the cleartext is
    -- printed exactly once at creation.
    secret_hash   bytea NOT NULL,
    site_id       uuid NOT NULL REFERENCES sites(id),
    created_by    text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    used_at       timestamptz,
    used_by_agent uuid REFERENCES agents(id),
    -- sha256 of the CSR that consumed this token. A retry presenting the
    -- SAME token and SAME CSR is an idempotent replay (the enroll response
    -- was lost in transit), not a token reuse.
    used_csr_hash bytea
);

CREATE TABLE certificates (
    serial     numeric PRIMARY KEY,
    agent_id   uuid NOT NULL REFERENCES agents(id),
    not_before timestamptz NOT NULL,
    not_after  timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz
);
CREATE INDEX certificates_agent_idx ON certificates (agent_id);

-- ---- probe configuration and results ----

-- Everything probeable, mesh peers included. kind='agent' rows are created
-- automatically at enrollment; kind='external' rows come from the admin CLI.
CREATE TABLE targets (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind       text NOT NULL CHECK (kind IN ('agent', 'external')),
    -- Admin CLI handle for external targets; synthesized for agent targets.
    name       text NOT NULL UNIQUE,
    agent_id   uuid REFERENCES agents(id),
    address    text NOT NULL DEFAULT '',
    -- 0 is the "unset" sentinel; defense in depth behind the shared
    -- targetadmin validation (CLI + HTTP API).
    port       int  NOT NULL DEFAULT 0,
    -- For HTTP probes: full URL.
    url        text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((kind = 'agent') = (agent_id IS NOT NULL)),
    CONSTRAINT targets_port_range CHECK (port >= 0 AND port <= 65535)
);
CREATE UNIQUE INDEX targets_agent_uidx ON targets (agent_id) WHERE agent_id IS NOT NULL;

CREATE TABLE mesh_groups (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE mesh_members (
    mesh_id uuid NOT NULL REFERENCES mesh_groups(id) ON DELETE CASCADE,
    site_id uuid NOT NULL REFERENCES sites(id),
    PRIMARY KEY (mesh_id, site_id)
);

-- A row is EITHER a direct probe (site_id + target_id: every agent at the
-- site runs it) OR a mesh template (mesh_id: meshexpand expands it over
-- ordered site pairs into per-agent specs).
CREATE TABLE probe_configs (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    site_id          uuid REFERENCES sites(id),
    target_id        uuid REFERENCES targets(id),
    mesh_id          uuid REFERENCES mesh_groups(id) ON DELETE CASCADE,
    -- polarbeam.v1.ProbeType numeric value.
    probe_type       smallint NOT NULL CHECK (probe_type > 0),
    interval_ms      int NOT NULL CHECK (interval_ms > 0),
    timeout_ms       int NOT NULL CHECK (timeout_ms > 0),
    train_count      int NOT NULL DEFAULT 0,
    train_spacing_ms int NOT NULL DEFAULT 0,
    params           jsonb NOT NULL DEFAULT '{}',
    enabled          boolean NOT NULL DEFAULT true,
    created_at       timestamptz NOT NULL DEFAULT now(),
    -- Audit trail for admin edits (CLI writes 'cli', the web UI writes the
    -- session username).
    updated_at       timestamptz NOT NULL DEFAULT now(),
    updated_by       text NOT NULL DEFAULT '',
    CHECK ((mesh_id IS NOT NULL AND site_id IS NULL AND target_id IS NULL)
        OR (mesh_id IS NULL AND site_id IS NOT NULL AND target_id IS NOT NULL))
);

-- Raw measurement hypertable. agent_id always comes from the mTLS identity
-- server-side, never from message fields. Timing columns are int
-- microseconds (NULL = not measured, wire -1); int caps at ~35 minutes,
-- far beyond any probe timeout. No FKs: ingest hot path.
CREATE TABLE probe_results (
    time       timestamptz NOT NULL,
    agent_id   uuid NOT NULL,
    target_id  uuid NOT NULL,
    probe_id   uuid NOT NULL,
    -- polarbeam.v1.ProbeType / ProbeStatus numeric values.
    probe_type smallint NOT NULL,
    status     smallint NOT NULL,
    sent       int NOT NULL DEFAULT 0,
    received   int NOT NULL DEFAULT 0,
    loss_pct   real,
    rtt_min_us    int,
    rtt_avg_us    int,
    rtt_max_us    int,
    rtt_stddev_us int,
    jitter_us     int,
    dns_us           int,
    tcp_connect_us   int,
    tls_handshake_us int,
    ttfb_us          int,
    total_us         int,
    -- Truncated human-readable failure reason; NULL on OK.
    error      text
);

SELECT create_hypertable('probe_results', 'time', chunk_time_interval => interval '1 day');

CREATE INDEX probe_results_series_idx
    ON probe_results (agent_id, target_id, probe_type, time DESC);

-- Spool replay is at-least-once; duplicates are dropped on insert
-- (ON CONFLICT DO NOTHING against this index).
CREATE UNIQUE INDEX probe_results_dedupe_uidx
    ON probe_results (agent_id, probe_id, time);

-- ---- dashboard users and PG-backed sessions ----

CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    username      text NOT NULL UNIQUE,
    -- PHC-encoded argon2id ($argon2id$v=19$m=...,t=...,p=...$salt$hash).
    -- NULL for federated users: their identity is the immutable OIDC
    -- subject, they have no password.
    password_hash text,
    role          text NOT NULL CHECK (role IN ('admin', 'viewer')),
    disabled      boolean NOT NULL DEFAULT false,
    auth_source   text NOT NULL DEFAULT 'local'
        CHECK (auth_source IN ('local', 'oidc')),
    oidc_subject  text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    -- Exactly one credential shape per source: local rows keep a hash and
    -- no subject, oidc rows the reverse. A row violating this is a server
    -- bug and must be loud.
    CONSTRAINT users_auth_shape CHECK (
        (auth_source = 'local' AND password_hash IS NOT NULL AND oidc_subject IS NULL)
     OR (auth_source = 'oidc'  AND password_hash IS NULL     AND oidc_subject IS NOT NULL)
    )
);
CREATE UNIQUE INDEX users_oidc_subject_idx
    ON users (oidc_subject) WHERE oidc_subject IS NOT NULL;

CREATE TABLE sessions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- sha256 of the random bearer token; the cleartext exists only in the
    -- browser cookie (same pattern as join_tokens.secret_hash — a DB dump
    -- never yields usable sessions).
    token_hash   bytea NOT NULL UNIQUE,
    user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- Per-session CSRF token, returned to the SPA by login and /auth/me.
    csrf_token   text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    -- Absolute expiry; no sliding idle window (MVP).
    expires_at   timestamptz NOT NULL,
    last_used_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sessions_expires_idx ON sessions (expires_at);

-- Shared dashboard settings: exactly one row (id is an always-true bool PK).
-- Thresholds drive map dot/line severity; edited from the SPA by admins.
-- The CHECKs mirror httpapi validation — hitting one from the API means a
-- handler bug, and it should be loud.
CREATE TABLE dashboard_settings (
    id              boolean PRIMARY KEY DEFAULT true CHECK (id),
    latency_warn_us bigint NOT NULL DEFAULT 100000,
    latency_crit_us bigint NOT NULL DEFAULT 250000,
    loss_warn_pct   double precision NOT NULL DEFAULT 1,
    loss_crit_pct   double precision NOT NULL DEFAULT 5,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    updated_by      text NOT NULL DEFAULT '',
    CHECK (latency_warn_us > 0 AND latency_crit_us > latency_warn_us),
    CHECK (loss_warn_pct >= 0 AND loss_crit_pct > loss_warn_pct AND loss_crit_pct <= 100)
);
-- Seeded here so GET never needs a missing-row branch and UPDATE always hits.
INSERT INTO dashboard_settings DEFAULT VALUES;

-- Optional OIDC single sign-on: the single-row IdP configuration edited
-- from Settings -> Authentication. OIDC is default-off; local (password)
-- accounts keep working regardless of this table's state. Same
-- always-true-bool PK trick as dashboard_settings. The CHECK mirrors
-- httpapi validation: enabled requires the fields the authorization-code
-- flow cannot run without.
CREATE TABLE oidc_settings (
    id             boolean PRIMARY KEY DEFAULT true CHECK (id),
    enabled        boolean NOT NULL DEFAULT false,
    issuer         text    NOT NULL DEFAULT '',
    client_id      text    NOT NULL DEFAULT '',
    client_secret  text    NOT NULL DEFAULT '',
    redirect_url   text    NOT NULL DEFAULT '',
    scopes         text[]  NOT NULL DEFAULT '{openid,profile,email}',
    username_claim text    NOT NULL DEFAULT 'preferred_username',
    role_claim     text    NOT NULL DEFAULT 'groups',
    admin_values   text[]  NOT NULL DEFAULT '{}',
    -- PEM blob, not a file path: the server container filesystem is
    -- immutable, and DB-stored config must be self-contained.
    ca_pem         text    NOT NULL DEFAULT '',
    updated_at     timestamptz NOT NULL DEFAULT now(),
    updated_by     text    NOT NULL DEFAULT '',
    CHECK (NOT enabled OR (issuer <> '' AND client_id <> '' AND client_secret <> '' AND redirect_url <> ''))
);
INSERT INTO oidc_settings DEFAULT VALUES;

-- ---- outage detection and traceroute path watching ----
--
-- A series is (agent_id, probe_id): the probe_id already pins target and
-- type (mesh probe IDs are deterministic UUIDv5), matching the dedupe index
-- on probe_results. target_id/probe_type are denormalized copies for
-- display. Like probe_results, these tables sit on the ingest hot path and
-- carry no FKs to agents/targets — mesh probe_ids have no backing row
-- anywhere. The single FK (series_state → outage_events) is load-bearing
-- for "exactly one open event per series".

CREATE TABLE outage_events (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind       text NOT NULL CHECK (kind IN ('probe_failing', 'agent_offline')),
    agent_id   uuid NOT NULL,
    probe_id   uuid,               -- NULL for agent_offline
    target_id  uuid,               -- NULL for agent_offline
    probe_type smallint,           -- NULL for agent_offline
    -- opened_at is the start of the failure streak (first failure / last
    -- evidence of life), not the moment the threshold was crossed.
    opened_at  timestamptz NOT NULL,
    closed_at  timestamptz,        -- NULL = still open
    -- error text of a failure in the opening streak, for display.
    open_error text
);

CREATE INDEX outage_events_recent_idx ON outage_events (opened_at DESC);
CREATE INDEX outage_events_open_idx ON outage_events (agent_id) WHERE closed_at IS NULL;
-- Belt and braces for "exactly one open event": per failing series, and per
-- offline agent. Racing openers resolve here, not in application logic.
CREATE UNIQUE INDEX outage_events_probe_open_uidx ON outage_events (agent_id, probe_id)
    WHERE kind = 'probe_failing' AND closed_at IS NULL;
CREATE UNIQUE INDEX outage_events_offline_open_uidx ON outage_events (agent_id)
    WHERE kind = 'agent_offline' AND closed_at IS NULL;

-- Restart-durable hysteresis counters, one row per series, updated inside
-- the ingest transaction. last_time orders spool replays: results at or
-- before it are ignored, so re-pushed batches can never double-count.
CREATE TABLE series_state (
    agent_id      uuid NOT NULL,
    probe_id      uuid NOT NULL,
    target_id     uuid NOT NULL,
    probe_type    smallint NOT NULL,
    consec_fails  int NOT NULL DEFAULT 0,
    consec_oks    int NOT NULL DEFAULT 0,
    first_fail_at timestamptz,     -- start of the current failure streak
    first_ok_at   timestamptz,     -- start of the current success streak
    last_status   smallint NOT NULL,
    last_time     timestamptz NOT NULL,
    open_event_id uuid REFERENCES outage_events(id),
    PRIMARY KEY (agent_id, probe_id)
);

-- Latest complete traceroute path per series. Hops mirror the wire Hop
-- message: [{"ttl": 1, "addrs": ["10.0.0.1"], "rtt_us": [311, 290]}, ...].
-- Traceroute hops live here and in path_events, never in the hypertable.
CREATE TABLE traceroute_current (
    agent_id     uuid NOT NULL,
    probe_id     uuid NOT NULL,
    target_id    uuid NOT NULL,
    updated_at   timestamptz NOT NULL,
    dest_reached boolean NOT NULL,
    path_hash    bytea NOT NULL,
    hops         jsonb NOT NULL,
    PRIMARY KEY (agent_id, probe_id)
);

CREATE TABLE path_events (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    time          timestamptz NOT NULL,
    agent_id      uuid NOT NULL,
    probe_id      uuid NOT NULL,
    target_id     uuid NOT NULL,
    old_path_hash bytea NOT NULL,
    new_path_hash bytea NOT NULL,
    old_hops      jsonb NOT NULL,
    new_hops      jsonb NOT NULL
);

CREATE INDEX path_events_time_idx ON path_events (time DESC);
