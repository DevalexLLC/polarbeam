# PolarBEAM — Architecture & Implementation Plan

## Context

PolarBEAM is an open-source project providing real-time visibility into connectivity, latency, packet loss, and service reachability between geographically dispersed sites. The core problem: inter-site network issues (drops, latency spikes, asymmetric routing/firewall failures) happen when nobody is watching, and there is no historical, directional record to diagnose them afterward.

Shape: a central control plane with a lightweight Go agent at each site (shipped as a container image; the wire contract is designed so future Windows/ESXi/Juniper agents are possible). Agents probe each other and designated endpoints, push results over mTLS on port 443, spool to disk when the control plane is unreachable, and pull their probe configuration from the control plane. A dashboard shows current + historical latency (min/avg/max/percentiles), packet loss, jitter, TCP connect and TLS handshake times, last successful test, recent outages, and traceroute path changes — **in both directions per site pair** — over 7/30/90/365-day windows.

## Decisions (confirmed with user)

- **Storage:** PostgreSQL + TimescaleDB; continuous aggregates for raw → hourly → daily; 365-day retention with percentiles.
- **Dashboard:** custom React/TypeScript SPA embedded in the server binary via `go:embed`.
- **mTLS:** built-in CA in the control plane; one-time join tokens; auto-rotating client certs.
- **Protocol:** gRPC over port 443 with mTLS (server-streamed config snapshots, batched result pushes).
- **Language:** Go for both binaries.
- **Agent form factor:** a **single static Go binary** (`polarbeam-agent`, built with `CGO_ENABLED=0`) — no runtime dependencies, no sidecars; it ships as a container image, and its only on-disk footprint is a config file plus `/var/lib/polarbeam-agent/{pki,spool}` which it creates itself.
- **Control plane deployment:** everything runs in containers (proxy + server + TimescaleDB) via compose; no bare-metal server install.
- **Fully vendored / zero external fetch at build time:** `vendor/` committed (`go build -mod=vendor`), generated proto code committed, and the **built SPA (`web/dist/`) committed** so `go build` never needs Node or npm. Container images are distributed as `docker save` tarballs for air-gapped import.

## Repo layout — single Go module monorepo

Module `github.com/devalexllc/polarbeam`. Everything a build needs lives in the repo — no network access at build time:
- `vendor/` committed; all builds use `go build -mod=vendor`.
- Generated proto code committed (`internal/pb/`); `make proto` regenerates (dev-only tooling) and CI diffs it.
- Built SPA committed (`web/dist/`); `go:embed` picks it up, so compiling the server needs only the Go toolchain. `make web` rebuilds it (Node needed only for frontend development, never for building/deploying).

```
polarbeam/
├── go.mod, Makefile, LICENSE (AGPL-3.0-only), buf.yaml, buf.gen.yaml
├── proto/polarbeam/v1/{common,enrollment,agent}.proto
├── internal/pb/polarbeamv1/            # committed generated code
├── cmd/polarbeam-server/               # subcommands: serve, ca init, migrate, user add, token create
├── cmd/polarbeam-agent/                # subcommands: run, enroll, selfcheck
├── internal/server/
│   ├── config/      # strict YAML + preflight (fail-loud: unknown keys = fatal)
│   ├── ca/          # built-in CA, CSR signing, revocation verify-callback
│   ├── grpcapi/     # EnrollmentService + AgentService
│   ├── httpapi/     # dashboard REST, sessions, CSRF
│   ├── store/       # pgx + sqlc
│   ├── meshexpand/  # mesh group → per-agent probe matrix
│   ├── outage/      # hysteresis state machine
│   ├── pathwatch/   # traceroute path-change detection
│   └── migrate/     # go:embed SQL migrations
├── internal/agent/
│   ├── config/ enroll/ uplink/ scheduler/ spool/
│   └── probes/{icmp,tcp,tls,http,dns,ntp,traceroute}/
├── vendor/                              # committed Go dependencies
├── web/                                 # Vite + React/TS SPA; dist/ committed; web/embed.go embeds dist/
├── deploy/
│   ├── compose/                         # production compose: proxy + server + timescaledb
│   ├── compose-dev/                     # dev overlay: + 3 fake agents + netem sidecar
│   ├── docker/                          # server/agent/proxy Dockerfiles (minimal bases)
│   ├── proxy/                           # nginx stream config: SNI passthrough on 443
│   ├── agent/                           # annotated agent.example.yaml
├── scripts/seed/                        # synthetic 90-day data generator
└── docs/{architecture,enrollment,airgap-build}.md
```

Admin CLI lives as `polarbeam-server` subcommands (needs DB + CA access anyway; one fewer artifact).

## gRPC surface (proto/polarbeam/v1)

Two services. Enrollment is server-TLS-only (join token authenticates); everything else requires mTLS.

```proto
service EnrollmentService {
  rpc Enroll(EnrollRequest) returns (EnrollResponse);        // join_token + CSR → cert + CA bundle + agent_id
}
service AgentService {                                        // identity = mTLS cert SAN, never message fields
  rpc StreamConfig(AgentHello) returns (stream ConfigSnapshot);  // full snapshots keyed by config_hash
  rpc PushResults(PushResultsRequest) returns (PushResultsResponse);
  rpc RenewCert(RenewCertRequest) returns (RenewCertResponse);
}
```

Key messages: `ProbeSpec` (probe_id UUID, type, target, interval, timeout, train_count/spacing, `map<string,string> params` for type-specific options); `ProbeResult` (probe_id, target_id, started_at, status enum incl. `UNSUPPORTED`, sent/received, RttStats min/avg/max/stddev in µs, jitter_us, Timings {dns, tcp_connect, tls_handshake, ttfb, total}, TracerouteResult {hops, dest_reached, path_hash}); `PushResultsRequest` reports spool overflow via `dropped_total` (lifetime total; the server folds an idempotent delta inside the ingest transaction, so retried pushes never double-count) plus the legacy `dropped_since_last_push` delta for pre-v0.4 servers (fail-loud loss accounting).

**Direction identity is unforgeable:** source = mTLS peer cert → agent → site; destination = `target_id` → (for mesh probes) peer agent → site. A→B and B→A are distinct series by construction.

Config delivery: server streams **full snapshots** (not diffs); agent diffs locally against running workers. The stream doubles as liveness (`last_seen_at`).

## TimescaleDB schema

Relational tables: `sites` (incl. optional `latitude`/`longitude` map coordinates, set via `site set` or Settings → Sites, both-or-neither), `agents` (incl. `probe_address` peers should target, `last_seen_at`), `targets` (kind `agent`|`external`), `probe_configs`, `mesh_groups` + `mesh_members`, `join_tokens` (sha256 hash, single-use, expiring), `certificates` (serial, revoked_at), `users` (argon2id) + `sessions` + `login_events` (append-only sign-in audit, one row per successful local or SSO login, kept forever; deliberately FK-less with username/auth_source snapshots so history and exact unique-user counts survive user deletion), `dashboard_settings` (single row: shared latency/loss warn+crit thresholds for the map) + `path_thresholds` (per-site-pair overrides of those thresholds — unordered pair keyed on site ids, per-field NULL = inherit the global value, cascades on site delete), `outage_events`, `series_state` (hysteresis counters, restart-durable), `path_events`, `traceroute_current`.

Raw hypertable `probe_results` (1-day chunks; narrow fixed-width columns: loss_pct, rtt min/avg/max/stddev µs, jitter, dns/tcp/tls/ttfb/total µs, plus one truncated `error` text — NULL on success — so failures are self-explanatory in the DB; traceroute hops go to `traceroute_current`/`path_events`, not the hypertable). Index `(agent_id, target_id, probe_type, time DESC)`; unique `(agent_id, probe_id, time)` so at-least-once spool replay dedupes on insert.

**Percentiles: TimescaleDB Toolkit `percentile_agg` (UddSketch)** — rollup-able, so daily aggregates derive from hourly and `approx_percentile(0.5|0.95|0.99, rollup(...))` answers any window. Toolkit is a hard dependency: preflight fails loud if the extension is missing (ships in `timescale/timescaledb-ha`).

Continuous aggregates: `probe_results_hourly` (from raw: samples, ok_samples, successful-only timing statistics partitioned by timing family, `percentile_agg`, jitter, sent/received, tcp/tls times) and `probe_results_daily` (rollup of hourly). Pair queries select one coherent family per direction—RTT first, then connect/application fallbacks, gated by a minimum-coverage floor (≥5% of the window's successful samples) so a freshly enabled prober cannot blank long-window history—while loss still folds all probes. Retention: raw 14d, hourly 100d, daily 400d. Window→source: 24h/7d→raw, 30/90d→hourly, 365d→daily; API responses include `resolution` and directional `latency_source` so charts are labeled honestly.

## Agent probe engine

- **Scheduler:** one worker per ProbeSpec; start offset `hash(probe_id) % interval` (splay; the uint32 hash is taken as nanoseconds, so the effective spread caps at ~4.3 s for longer intervals). New snapshot → diff by probe_id+spec-hash, stop/start/restart workers. Unknown probe type → log error + report `UNSUPPORTED` (never silently skip).
- **Prober interface:** `Run(ctx, spec) *pb.ProbeResult`, registry keyed by ProbeType.
- **ICMP:** unprivileged datagram ICMP first (`x/net/icmp` udp4, works when `net.ipv4.ping_group_range` covers the service group), fallback raw socket via `CAP_NET_RAW` (`--cap-add NET_RAW` on the container); `selfcheck` verifies at start. Trains of `train_count` (default 10) echoes spaced 200 ms → loss %, min/avg/max/stddev.
- **Jitter:** RFC 3550 smoothing `J += (|RTTᵢ−RTTᵢ₋₁|−J)/16` across consecutive RTTs, carried per-series across runs.
- **TCP/TLS:** timed `DialContext` + `tls.Client.HandshakeContext`. **HTTP(S):** `httptrace` → dns/tcp/tls/ttfb/total + expected-status assertion. **DNS:** `codeberg.org/miekg/dns` (v2), configurable resolver/qname/qtype, RCODE check.
- **NTP:** hand-rolled 48-byte SNTP client-mode request over UDP (stdlib only), single shot per run, default port 123; the transmit timestamp is a `crypto/rand` nonce so the originate-echo check authenticates the reply. Validates server mode, stratum 1–15, synchronized leap, nonzero transmit timestamp; Kiss-o'-Death (stratum 0) reports the kiss code. Reachability + RTT only — no clock-offset math. **Direct-only** (peer agents serve no time; `probeadmin.DirectOnly`).
- **Traceroute:** UDP with incrementing TTL, ICMP time-exceeded read on a **raw** ICMP socket — unprivileged datagram ICMP does not deliver errors elicited by another socket's packets, so traceroute strictly requires CAP_NET_RAW (missing capability = ERROR result every cadence, never a skip); 3 probes/hop, max 30 hops, slower interval (~5 min); `path_hash = sha256(hop IPs)`.

## Outage detection (server-side)

`series_state`-backed hysteresis: open `probe_failing` outage after **3 consecutive failures**, close after **3 consecutive successes** — updated in the ingest transaction. A 30 s sweep detects silence: no result in 3×interval + stale `last_seen_at` → single `agent_offline` event per agent (not per series). Restart-durable, no flapping. "Recent outages" = open events or ended within the query window.

## Agent disk spool

**Spool-first single path:** every batch is appended to segment files (`/var/lib/polarbeam-agent/spool/`, length-prefixed protobuf + CRC32, rotate at 1 MiB/60 s, fsync on rotation and before ack-delete); uplink reads from the head and deletes on server ack. Crash-safe, oldest-first replay. Bounds `spool.max_bytes` (256 MiB default) / `spool.max_age` (7 d): overflow drops oldest whole segments, logs at error, and reports the loss via a persisted lifetime `dropped_total` (plus the legacy `dropped_since_last_push` delta for old servers) — no silent loss, and the accounting is retry-idempotent: the server records only the portion of the total beyond the last one it saw, in the same transaction as the batch insert. Bounds are also enforced on recovered data at spool open, before the pusher wakes, so an agent restarted after a long outage never replays segments older than `max_age` (the control plane's aggregate-refresh windows assume replayed data is at most `max_age` old). A spool **write failure** (I/O error, full disk) is fatal: the agent logs it, exits non-zero, the container runtime's restart policy restarts it, and the entrypoint selfcheck blocks each restart loudly naming the unwritable spool until the disk clears — an agent that cannot spool is never silently "online". Per-record faults (a result that cannot be marshalled or exceeds the 1 MiB record limit) are not fatal — they take the same counted loss path, since retrying such a record can never succeed. Uplink batches ≤500 results or 5 s, exponential backoff (max 1 min), burst-drain on reconnect.

## Bidirectional pairing

`mesh_groups`+`mesh_members` express "these sites full-mesh." `meshexpand` expands each template over **every peer agent** into per-agent ProbeSpecs targeting the peers' `probe_address` (deterministic probe_id = UUIDv5(template_config_id, "src_site|target_id") → stable config hashes, distinct per template row, direction, and destination agent — N agents at a site are N series). UI: `GET /api/v1/pairs/{a}/{b}` returns `{a_to_b, b_to_a}`; SPA renders side-by-side dual-direction charts and paired matrix cells so asymmetry is visually obvious.

## Dashboard REST API + auth

`/api/v1/*` JSON, separate from agent gRPC. **Auth: local users + PG-backed sessions** (argon2id, HttpOnly/Secure/SameSite cookie, CSRF token; roles `admin`/`viewer`; first admin via `polarbeam-server user add --admin`) — air-gap-safe and revocable with no external dependency. **Optionally**, dashboard sign-in can additionally delegate to an OpenID Connect provider (authorization-code + PKCE, DB-stored config edited from Settings → Authentication, applied without restart): federated users are JIT-provisioned keyed on the immutable OIDC `sub`, a configurable claim maps to `admin`/`viewer`, and the IdP calls (discovery/token/JWKS) are the server's only outbound HTTP — lazy, bounded, and never on the local-login or startup path, so local accounts remain break-glass when the IdP is down. Builds stay fully offline; OIDC is runtime-only, admin-opted-in egress. Every successful sign-in (local or SSO) appends a `login_events` row, powering the admin-only Settings → Users view: all accounts with role, auth source, sign-in count, last sign-in, and 12 months of monthly totals.

Endpoints: auth (login/logout/me; `auth/providers` open advertisement; `auth/oidc/start` + `auth/oidc/callback` for SSO); `sites`, `agents` (sites carry `latitude`/`longitude`, null until placed); `matrix` (site×site grid, per-direction status plus per-probe checks); `settings` (GET any session, PUT admin-only: shared map thresholds; GET carries the per-pair `overrides` list); `settings/path-thresholds/{a}/{b}` (admin-only PUT/DELETE: per-site-pair threshold overrides, either site order addresses the same unordered pair); `settings/oidc` (admin-only GET/PUT + `settings/oidc/test` discovery check; the client secret is write-only); `pairs/{a}/{b}?window=` (both directions: current checks, coherent min/avg/max/p50/p95/p99, loss, jitter, tcp/tls, last_ok_at); `pairs/{a}/{b}/series?metric=&window=7d|30d|90d|365d` (directional latency sources); `outages`; `path-events`; `traceroute/{src}/{dst}`; admin CRUD for probe-configs/targets/mesh-groups/sites (`config/sites`; site delete refuses while agents/meshes/probes reference the site and removes unused join tokens with it); join-token issue/list/delete (`config/tokens`, admin-only including the list; deleting an unused token revokes it, used tokens are immutable audit records); `users` (admin-only: account inventory — including deleted identities reconstructed from sign-in snapshots — with login counts plus monthly sign-in totals; server-side username/role/status/source filters and limit/offset paging; POST creates a local user with a generated shown-once password, PUT toggles `disabled`, DELETE removes the account — self-service lockout is refused and the last enabled admin is protected); cert revoke (CLI/DB only).

## CA / cert lifecycle

- ECDSA P-256 CA, 10-year self-signed root via `polarbeam-server ca init` (refuses overwrite); preflight fails loud if serving without a CA.
- Agent certs: 30-day lifetime, identity in URI SAN `polarbeam://agent/<uuid>`; server signs CSRs — private keys never leave the agent.
- Enrollment: `polarbeam-server token create --site nyc --ttl 24h` prints `<id>.<secret>` once (DB stores sha256, single-use) → `polarbeam-agent enroll --server host:443 --token …` writes cert + CA bundle.
- Rotation: renew at 2/3 lifetime via `RenewCert` on the existing mTLS channel, retry daily; fully expired (dark >30 d) → re-enroll with fresh token, by design.
- Revocation: DB-backed — server's `VerifyPeerCertificate` checks serial against `certificates` (30 s cache); no CRL/OCSP since the control plane is the sole verifier. Live streams for revoked serials are dropped by the sweep.

## Deployment topology & port strategy

The control plane is **containers only**, fronted by a proxy:

```
                       :443 (only exposed port)
agents ──mTLS/gRPC──▶ ┌───────────┐  SNI: grpc.lh.example ──▶ server:8443 (gRPC, mTLS terminated by server)
browsers ──HTTPS────▶ │ nginx     │  SNI: lh.example ────────▶ server:8080 (dashboard HTTPS)
                       │ (stream/  │
                       │  SNI pass-│         server ──▶ timescaledb:5432 (internal network only)
                       └─through)──┘
```

- The proxy is nginx's stream module doing **SNI-based TCP passthrough** — it never terminates TLS, so agent client-cert verification stays end-to-end in the Go server (mTLS is not broken by the proxy) and no browser client-cert picker is triggered (different SNI names route to different internal listeners). Both agents and the dashboard share :443 externally. It prepends a **PROXY protocol v1 header** on both routes so the server sees real client addresses (per-IP login rate limiting; enrollment observed source) — passthrough alone preserves TLS, not the TCP source address.
- The server binary keeps two internal listeners (gRPC+mTLS, dashboard HTTPS) — it still works proxy-less for dev (`listen.proxy_protocol: false`, the default), and operators may substitute their own SNI-capable proxy (haproxy, traefik) since passthrough is generic, provided it also sends PROXY protocol to both backends (or the knob is turned off, degrading login rate limiting to one shared bucket).
- TimescaleDB is never exposed outside the compose network.
- Admin CLI runs inside the container: `docker compose exec server polarbeam-server token create …` (Makefile wrappers provided).

## Milestones

**M0 — Scaffolding (~½ wk).** go.mod, Makefile (`build test lint proto web vendor up down seed`), buf config, empty mains, compose file with `timescale/timescaledb-ha:pg16-all`, strict-YAML loaders + preflight stubs, CONTRIBUTING.md (Conventional Commits).
*Verify:* `make build` emits both binaries; `make up` starts Timescale; bad YAML key → non-zero exit naming the key.

**M1 — Proto, CA, enrollment, mTLS session (1–2 wk).** protos + committed pb, `ca`, `grpcapi`, `store`, migrations (sites/agents/join_tokens/certificates), agent `enroll`+`uplink`, StreamConfig registering hellos.
*Verify:* compose up → `token create` → `enroll` writes cert → `run` connects, `last_seen_at` updates; revoking the cert kills the connection.

**M2 — TCP/TLS/HTTP probes, config distribution, ingestion, spool (2 wk).** scheduler, tcp/tls/http probers, spool, `probe_results` hypertable, PushResults ingest, minimal probe-config CRUD.
*Verify:* TCP probe against compose Postgres lands rows with sane `tcp_connect_us`; stop server 5 min → spool → restart → full replay, no gap; overflow drops oldest and reports `dropped_since_last_push`.

**M3 — Dashboard MVP (2 wk).** httpapi (auth, sites, agents, matrix, pair series from raw), SPA (login, matrix, pair drill-down chart), `web/embed.go`, `user add`.
*Verify:* browser login through the proxy (dev: https://localhost:9443, self-signed cert); live matrix; pair chart updates.

**M4 — ICMP/DNS/traceroute, outage + path detection (2 wk).** icmp/dns/traceroute probers, jitter/loss, `outage`+`pathwatch`, related migrations, outages/path UI, agent container with CAP_NET_RAW.
*Verify:* `iptables … -j DROP` in an agent container → exactly one outage after 3 failures, closes after unblock + 3 successes; netem reroute → `path_events` row + UI diff.

**M5 — Aggregates, retention, percentiles, directional views (1–2 wk).** Toolkit extension + preflight, hourly/daily caggs, retention policies, window resolution, side-by-side A→B/B→A UI, p50/p95/p99, seed script.
*Verify:* `make seed` loads 90 d synthetic; 90 d pair query <500 ms from hourly agg; percentiles match seed distribution; 365 d renders from daily.

**M6 — Packaging, rotation, air-gap hardening (1–2 wk).** Published agent container image (non-root UID 10001, `cap_net_raw` file capability, selfcheck entrypoint); control-plane release bundle = `docker save` image tarballs (server, proxy, timescaledb) + production compose file + docs; cert-rotation e2e (short-lifetime test mode); `selfcheck`; CI check that builds with the network disabled (proves vendoring is complete).
*Verify:* agent image loads on a host with no internet and unprivileged ICMP works; control-plane bundle imports via `docker load` and starts on an offline host; 10-min cert lifetime → observed auto-renewal; `GOFLAGS=-mod=vendor GOPROXY=off make build` succeeds.

## Dev environment

Dev compose = production compose (`deploy/compose/`: proxy + server + timescaledb) plus a dev overlay (`deploy/compose-dev/`): `agent-{nyc,lon,syd}` containers (a one-shot bootstrap service mints dev tokens and seeds a dashboard login `admin`/`polarbeam-dev`; `cap_add: NET_RAW`), optional netem profile (`delay 80ms loss 2%`) for realistic charts. Using the same base compose in dev exercises the SNI-passthrough path continuously. `make dev` runs the server locally + Vite proxy for SPA hot-reload; `make up` brings up the full containerized stack.

**Note:** `make up` must always include the dev overlay in dev environments — a prior lesson: restarting only the core compose silently drops overlay services (fake agents' tokens, monitoring). Makefile targets should be explicit about which files they compose.

## Verification (end-to-end)

Each milestone has its own gate above; the standing harness is `deploy/docker-compose.yml` — three agents meshed across fake "sites" with netem-induced latency/loss, exercised after every change (`make up && make seed`, then check matrix, pair drill-down, outage injection via iptables, spool replay via server stop/start).
