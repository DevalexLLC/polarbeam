# PolarBEAM — Architecture & Implementation Plan

## Context

PolarBEAM is an open-source project providing real-time visibility into connectivity, latency, packet loss, and service reachability between geographically dispersed sites. The core problem: inter-site network issues (drops, latency spikes, asymmetric routing/firewall failures) happen when nobody is watching, and there is no historical, directional record to diagnose them afterward.

Shape: a central control plane with a lightweight Go agent at each site (shipped as a container image; the wire contract is designed so future Windows/ESXi/Juniper agents are possible). Agents probe each other and designated endpoints, push results over mTLS on port 443, spool to disk when the control plane is unreachable, and pull their probe configuration from the control plane. A dashboard shows current + historical latency (min/avg/max/percentiles), packet loss, jitter, TCP connect and TLS handshake times, last successful test, recent outages, and traceroute path changes — **in both directions per site pair** — over 7/30/90/365-day windows.

## Decisions (confirmed with user)

- **Storage:** PostgreSQL + TimescaleDB; continuous aggregates for raw → hourly → daily; 365-day retention with percentiles.
- **Dashboard:** custom React/TypeScript SPA embedded in the server binary via `go:embed`.
- **mTLS:** built-in CA in the control plane; one-time join tokens; auto-rotating client certs.
- **Protocol:** gRPC over port 443 with mTLS (server-streamed config snapshots, batched result pushes). The gRPC listener enforces TLS 1.3 with hybrid ML-KEM (post-quantum) key exchange only — no classical fallback. Shipped Go agents explicitly offer X25519MLKEM768, SecP256r1MLKEM768, and SecP384r1MLKEM1024 for enrollment and uplink connections; future non-Go agents' TLS stacks must support at least one of those groups. Authentication is post-quantum too: the built-in CA issues ML-DSA-65 (FIPS 204) certificates by default (`ca init --algorithm ecdsa-p256` remains as a classical escape hatch), so both halves of the transport resist a future quantum adversary. The dashboard listener stays classical — operators bring their own certificates and browsers do not support ML-DSA.
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
├── cmd/polarbeam-server/               # subcommands: serve, ca init (--algorithm), migrate, user add (--admin | --role/--network), token create
├── cmd/polarbeam-agent/                # subcommands: run, enroll, selfcheck
├── internal/server/
│   ├── config/      # strict YAML + preflight (fail-loud: unknown keys = fatal)
│   ├── ca/          # built-in CA, CSR signing, revocation verify-callback
│   ├── grpcapi/     # EnrollmentService + AgentService
│   ├── httpapi/     # dashboard REST, sessions, CSRF
│   ├── store/       # hand-written pgx queries
│   ├── meshexpand/  # mesh group → per-agent probe matrix
│   ├── outage/      # hysteresis state machine
│   ├── pathwatch/   # traceroute path-change detection
│   ├── seed/        # synthetic 90-day data generator
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
└── docs/{architecture,install,probes,airgap-build}.md
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

Relational tables: `sites` (incl. optional `latitude`/`longitude` map coordinates, set via `site set` or Settings → Sites, both-or-neither), `networks` (named connectivity planes — `name` unique and immutable, seeded `default` row that cannot be deleted; a trust statement "agents on this name can reach each other", not IPAM), `agents` (incl. `probe_address` peers should target, `last_seen_at`, and `network_id NOT NULL` — inherited from the join token at enrollment, immutable after; move a box by re-enrolling), `targets` (kind `agent`|`external`), `probe_configs` (direct rows carry `network_id`; mesh rows inherit the mesh's), `mesh_groups` (incl. `network_id NOT NULL` — a mesh binds exactly one plane) + `mesh_members`, `join_tokens` (sha256 hash, single-use, expiring, incl. `network_id`), `certificates` (serial, revoked_at), `users` (argon2id) + `sessions` + `login_events` (append-only sign-in audit, one row per successful local or SSO login, kept forever; deliberately FK-less with username/auth_source snapshots so history and exact unique-user counts survive user deletion), `dashboard_settings` (single row: shared latency/loss warn+crit thresholds for the map) + `path_thresholds` (per-site-pair overrides of those thresholds — unordered pair keyed on site ids, per-field NULL = inherit the global value, cascades on site delete), `banner_settings` (single row: optional shared marking text + enable flag, rendered in bands at the top and bottom of every dashboard screen), `outage_events`, `series_state` (hysteresis counters, restart-durable), `path_events`, `traceroute_current`.

Raw hypertable `probe_results` (1-day chunks; narrow fixed-width columns: loss_pct, rtt min/avg/max/stddev µs, jitter, dns/tcp/tls/ttfb/total µs, plus one truncated `error` text — NULL on success — so failures are self-explanatory in the DB; traceroute hops go to `traceroute_current`/`path_events`, not the hypertable). Index `(agent_id, target_id, probe_type, time DESC)`; unique `(agent_id, probe_id, time)` so at-least-once spool replay dedupes on insert. Migration 0011 adds supporting B-trees elsewhere: FK/cleanup lookups (`probe_configs`, `mesh_members`, `sessions`, `join_tokens`, probe-keyed `series_state`/`traceroute_current`/`outage_events` scans, the sweep's `open_event_id` join, `outage_events.closed_at`) and composite `(agent_id, target_id, bucket DESC)` pair indexes on the hourly/daily caggs.

**Percentiles: TimescaleDB Toolkit `percentile_agg` (UddSketch)** — rollup-able, so daily aggregates derive from hourly and `approx_percentile(0.5|0.95|0.99, rollup(...))` answers any window. Toolkit is a hard dependency: preflight fails loud if the extension is missing (ships in `timescale/timescaledb-ha`).

Continuous aggregates: `probe_results_hourly` (from raw: samples, ok_samples, successful-only timing statistics partitioned by timing family, `percentile_agg`, jitter, sent/received, tcp/tls times) and `probe_results_daily` (rollup of hourly). Pair queries select one coherent family per direction—RTT first, then connect/application fallbacks, gated by a minimum-coverage floor (≥5% of the window's successful samples) so a freshly enabled prober cannot blank long-window history—while loss still folds all probes. A third, narrow cagg `probe_results_health_30m` (30-min buckets × agent × probe series: samples/ok_samples, 14d retention) serves the agent health strips (`/api/v1/agents/health`, `/api/v1/agents/{id}/health`), replacing per-poll 24 h raw scans. A fourth/fifth pair, `probe_results_stage_hourly`/`_daily` (bucket × agent × target × probe_type: successful-only sum/count per stage — dns, tcp_connect, tls_handshake, ttfb, total), serves the target detail page's stage-breakdown chart; they are siblings of the latency caggs (those shipped and are immutable) and on upgrade backfill only from raw still inside its 14d retention, so long-window stage charts fill forward over time. Retention: raw 14d, hourly 100d, daily 400d (stage caggs match). Window→source: 24h/7d→raw, 30/90d→hourly, 365d→daily; API responses include `resolution` and directional `latency_source` so charts are labeled honestly.

## Agent probe engine

- **Scheduler:** one worker per ProbeSpec; start offset `hash(probe_id) % interval` (splay; the uint32 hash is taken as nanoseconds, so the effective spread caps at ~4.3 s for longer intervals). New snapshot → diff by probe_id+spec-hash, stop/start/restart workers. Unknown probe type → log error + report `UNSUPPORTED` (never silently skip).
- **Prober interface:** `Run(ctx, spec) *pb.ProbeResult`, registry keyed by ProbeType.
- **ICMP:** unprivileged datagram ICMP first (`x/net/icmp` udp4, works when `net.ipv4.ping_group_range` covers the service group), fallback raw socket via `CAP_NET_RAW` (`--cap-add NET_RAW` on the container); `selfcheck` verifies at start. Trains of `train_count` (default 10) echoes spaced 200 ms → loss %, min/avg/max/stddev.
- **Jitter:** RFC 3550 smoothing `J += (|RTTᵢ−RTTᵢ₋₁|−J)/16` across consecutive RTTs, carried per-series across runs.
- **TCP/TLS:** timed `DialContext` + `tls.Client.HandshakeContext`. **HTTP(S):** `httptrace` → dns/tcp/tls/ttfb/total + expected-status assertion. **DNS:** `codeberg.org/miekg/dns` (v2), configurable resolver/qname/qtype, RCODE check.
- **NTP:** hand-rolled 48-byte SNTP client-mode request over UDP (stdlib only), single shot per run, default port 123; the transmit timestamp is a `crypto/rand` nonce so the originate-echo check authenticates the reply. Validates server mode, stratum 1–15, synchronized leap, nonzero transmit timestamp; Kiss-o'-Death (stratum 0) reports the kiss code. Reachability + RTT only — no clock-offset math. **Direct-only** (peer agents serve no time; `probeadmin.DirectOnly`).
- **Traceroute:** UDP with incrementing TTL, ICMP time-exceeded read on a **raw** ICMP socket — unprivileged datagram ICMP does not deliver errors elicited by another socket's packets, so traceroute strictly requires CAP_NET_RAW (missing capability = ERROR result every cadence, never a skip); 3 probes/hop, max 30 hops, slower interval (~5 min); `path_hash = sha256(hop IPs)`.
- **Path MTU:** ICMP echoes padded to candidate sizes with DF enforced (`x/sys/unix` `IP_PMTUDISC_PROBE` / `IPV6_DONTFRAG`, Linux-only — other platforms report ERROR), bounded binary search between `mtu.min`/`mtu.max` (defaults 1280/1500), 3 sends per size on a **raw** ICMP socket (CAP_NET_RAW strictly required, traceroute rationale); FragNeeded/PTB bounds the search and its advertised next-hop MTU is verified directly; silence above a proven-good size = suspected PMTU black hole → TIMEOUT, never healthy. All sizes are IP-packet bytes incl. header; results go to `path_mtu_current`/`path_mtu_events` via `mtuwatch` (traceroute-style side tables), never into latency columns.

## Outage detection (server-side)

`series_state`-backed hysteresis: open `probe_failing` outage after **3 consecutive failures**, close after **3 consecutive successes** — updated in the ingest transaction. The same machinery opens a `probe_degraded` event after **3 consecutive successes breaching the critical latency/loss thresholds** (the effective per-direction values: global `dashboard_settings` merged with the site-pair `path_thresholds` override; external targets grade on the global values; warn-tier breaches stay live-view-only) and closes it after 3 consecutive clean successes. Grading happens at ingest against then-current thresholds (edits converge within the 30 s assignment-cache TTL; spool replay grades at replay time). At most one probe event is open per series — down supersedes degraded: a failing streak closes an open degraded event and opens `probe_failing` at the same instant, and vice versa on recovery into a still-breaching link. A 30 s sweep detects silence: no result in 3×interval + stale `last_seen_at` → single `agent_offline` event per agent (not per series). Restart-durable, no flapping. "Recent outages" = open events or ended within the query window.

## Agent disk spool

**Spool-first single path:** every batch is appended to segment files (`/var/lib/polarbeam-agent/spool/`, length-prefixed protobuf + CRC32, rotate at 1 MiB/60 s, fsync on rotation and before ack-delete); uplink reads from the head and deletes on server ack. Crash-safe, oldest-first replay. Bounds `spool.max_bytes` (256 MiB default) / `spool.max_age` (7 d): overflow drops oldest whole segments, logs at error, and reports the loss via a persisted lifetime `dropped_total` (plus the legacy `dropped_since_last_push` delta for old servers) — no silent loss, and the accounting is retry-idempotent: the server records only the portion of the total beyond the last one it saw, in the same transaction as the batch insert. Bounds are also enforced on recovered data at spool open, before the pusher wakes, so an agent restarted after a long outage never replays segments older than `max_age` (the control plane's aggregate-refresh windows assume replayed data is at most `max_age` old). A spool **write failure** (I/O error, full disk) is fatal: the agent logs it, exits non-zero, the container runtime's restart policy restarts it, and the entrypoint selfcheck blocks each restart loudly naming the unwritable spool until the disk clears — an agent that cannot spool is never silently "online". Per-record faults (a result that cannot be marshalled or exceeds the 1 MiB record limit) are not fatal — they take the same counted loss path, since retrying such a record can never succeed. Uplink batches ≤500 results or 5 s, exponential backoff (max 1 min), burst-drain on reconnect.

## Bidirectional pairing

`mesh_groups`+`mesh_members` express "these sites full-mesh on one network." `meshexpand` expands each template over **every same-network peer agent** — agents at member sites on a different plane are never paired, so two meshes on different networks coexist at the same sites without ever probing across the boundary — into per-agent ProbeSpecs targeting the peers' `probe_address` (deterministic probe_id = UUIDv5(template_config_id, "src_site|target_id") → stable config hashes, distinct per template row, direction, and destination agent — N agents at a site are N series). UI: `GET /api/v1/pairs/{a}/{b}` returns `{a_to_b, b_to_a}`; SPA renders side-by-side dual-direction charts and paired matrix cells so asymmetry is visually obvious. The complementary target drill-down (`#/target/{id}`, linked from Settings → Targets and the Agents probe rows) turns the axis around: every site probing one target — external targets' only metrics surface, and for agent targets the many-sources-to-one-destination view the pair page can't show — plus a DNS/TCP/TLS/TTFB/total stage-breakdown chart where probes measure those.

## Dashboard REST API + auth

Collection endpoints that support scalable inventories share one optional
query contract: `network`, trimmed `q` (at most 200 characters), endpoint-
specific filters, allow-listed `sort`, `order=asc|desc`, `limit` from 1 to
100, and a nonnegative `offset`. Supplying any of those parameters enables
query mode (default limit 100); the response keeps its endpoint-specific
collection key and adds `page` with `limit`, `offset`, `total`, and
`has_more`. Omitting them preserves the legacy full response. Explicit
networks can only narrow the authenticated scope, and unknown or inaccessible
names return the same 404 shape. Empty `network` and `q` values still enter
query mode but apply no narrowing or search; every other present list
parameter must carry a nonempty value valid for its allowlist or numeric
range.

`GET /api/v1/path-events` query mode searches route-change display evidence,
sorts by time, agent, source, destination, or changed-hop count, and returns
stable event/agent/probe/target IDs. `changed_hops` compares deduplicated
address sets at each TTL (including added or removed TTLs). The server first
caps the newest 500 matching events, so `page.total` never exceeds 500 and
`truncated` reports additional matches outside that safety window. Unlike the
legacy window-only response described in the endpoint inventory below, query
mode retains `target_id` after its display row is deleted.

`GET /api/v1/agents` query mode returns a stable page plus filtered health
summary (`offline`, `degraded`, `healthy`, `no_data`); requests without list
parameters retain the original complete-feed response. `GET /api/v1/targets`
is the paginated operational inventory, distinct from administrative
`/config/targets`: it joins target and agent identities, enabled and total
probe assignments, distinct probing sites, and open incidents in SQL. Both
inventories apply tenant scope before search, counts, or summaries, including
activity against operator-published global targets. The target response keeps
both a filtered `summary` and an unfiltered `scope_summary` for header context;
an explicit network includes a global external target only when an enabled
direct probe assigns it to that plane.

`/api/v1/*` JSON, separate from agent gRPC. **Auth: local users + PG-backed sessions** (argon2id, HttpOnly/Secure/SameSite cookie, CSRF token; four roles — global `admin`/`viewer` plus network-scoped `network_admin`/`network_viewer`, whose visibility and writes are limited to an explicit set of networks; first admin via `polarbeam-server user add --admin`, scoped accounts via `user add --role <role> --network <name>`) — air-gap-safe and revocable with no external dependency. **Optionally**, dashboard sign-in can additionally delegate to an OpenID Connect provider (authorization-code + PKCE, DB-stored config edited from Settings → Authentication, applied without restart): federated users are JIT-provisioned keyed on the immutable OIDC `sub`, a configurable claim maps to `admin` (via `admin_values`) or to a network-scoped role (via ordered `role_rules`, strongest role winning and networks unioned) with `unmatched_role` deciding whether a user matching nothing becomes a global viewer or is denied — any authorization-policy change revokes federated sessions, since only a login remaps them — and the IdP calls (discovery/token/JWKS) are the server's only outbound HTTP — lazy, bounded, and never on the local-login or startup path, so local accounts remain break-glass when the IdP is down. Builds stay fully offline; OIDC is runtime-only, admin-opted-in egress. Every successful sign-in (local or SSO) appends a `login_events` row, powering the admin-only Settings → Users view: all accounts with role, auth source, sign-in count, last sign-in, and 12 months of monthly totals.

Dispositions are four: open (no session), any-session reads (scope-filtered server-side for the network-scoped roles, never 403), admin-only behind an exact `admin` compare, and **network-scoped** behind a separate `networkWrite` guard admitting `admin` or `network_admin` — admission is not authorization, so each such handler additionally proves the touched resource's plane and answers 404, worded exactly like a name that never existed, so the write surface cannot enumerate other tenants. Endpoints: auth (login/logout/me — `me` carries the caller's role and network scope, the SPA's capability signal; `auth/providers` open advertisement; `auth/oidc/start` + `auth/oidc/callback` for SSO); `sites`, `agents` (sites carry `latitude`/`longitude`, null until placed); `matrix` (site×site grid, per-direction status plus per-probe checks; each cell also carries per-network sub-cells folded with the same rule — the top-level cell stays the all-plane fold, so single-network consumers see an unchanged shape); `settings` (GET any session, PUT admin-only: shared map thresholds; GET carries the per-pair `overrides` list and the per-network `network_defaults` list, both scope-filtered); `settings/path-thresholds/{a}/{b}` (network-scoped PUT/DELETE: per-site-pair threshold overrides, either site order addresses the same unordered pair; the plane rides as `?network=`, and the all-planes row — no `?network=` — grades every tenant and stays admin-only) with `settings/network-thresholds/{network}` (network-scoped PUT/DELETE: the per-plane default layer between the global row and the pair overrides; resolution is per metric, most specific first — pair+network, pair, network, global); `settings/oidc` (admin-only GET/PUT, plus `settings/oidc/test` discovery check; the client secret is write-only, and so are the tenant-policy fields `role_rules` and `unmatched_role` in the sense that omitting either keeps the stored value, resolved under the settings row lock, so an unrelated save cannot strip a mapping); `ui-banner` (open read: banner enable flag + text, text blank while disabled) with `settings/ui-banner` (admin-only GET/PUT); `pairs/{a}/{b}?window=` (both directions: current checks, coherent min/avg/max/p50/p95/p99, loss, jitter, tcp/tls, last_ok_at; lists the pair's networks and takes an optional `?network=` filter — unknown name is a loud 404, never a silent all-planes fallback — which also applies to `/series`, `traceroute`, and `path-mtu`); `pairs/{a}/{b}/series?metric=&window=7d|30d|90d|365d` (directional latency sources); `targets/{id}` + `targets/{id}/series` + `targets/{id}/stages` + `targets/{id}/health` (the target detail page — any target kind: per-source-site summaries/series reusing the pair machinery with the single target as destination, per-stage timing averages from the stage caggs, and fixed-24h per-probe health strips whose slot drill-down reuses `agents/{agent_id}/health/bucket`); `outages`; `path-events` (rows carry `target_id`, null once the target row is deleted, so the SPA can link route-change destinations); `traceroute/{src}/{dst}`; network-scoped CRUD for probe-configs/targets/mesh-groups (each write proves the touched plane; targets carry an owner — NULL is a global, operator-published row readable everywhere but writable only by an admin, a set network makes the row a tenant's) and admin-only CRUD for sites (`config/sites`, shared vocabulary; site delete refuses while agents/meshes/probes reference the site and removes unused join tokens with it) and networks (`config/networks`: reads for any session — the SPA's selector options — writes admin-only; only `display_name` is editable, the name is immutable; delete refuses while agents/meshes/probes reference the network, sweeps unused join tokens with it, and `default` is undeletable); join-token issue/list/delete (`config/tokens`, network-scoped including the list — the token carries the network, so an agent lands on the tenant's plane by construction and never chooses it; deleting an unused token revokes it, used tokens are immutable audit records); `users` (admin-only, carrying each account's role and network scope; role is set at create, scope editable afterwards for local scoped accounts only: account inventory — including deleted identities reconstructed from sign-in snapshots — with login counts plus monthly sign-in totals; server-side username/role/status/source filters and limit/offset paging; POST creates a local user with a generated shown-once password, PUT toggles `disabled`, DELETE removes the account — self-service lockout is refused and the last enabled admin is protected); cert revoke (CLI/DB only).

## CA / cert lifecycle

- ML-DSA-65 CA by default (`ca init --algorithm mldsa65|ecdsa-p256`), 10-year self-signed root via `polarbeam-server ca init` (refuses overwrite); preflight fails loud if serving without a CA. Issued leaf keys mirror the root's algorithm — agents derive theirs from the trust anchor at enrollment, and the CA refuses CSRs whose key algorithm differs from the root's (a classical leaf under a PQ root would silently weaken that identity). Keys are stored as PKCS#8, with pre-cutover SEC1 ECDSA keys still readable. ML-DSA-65 grows the handshake by roughly 20 KB each way (~5.5 KB certs, 3.3 KB signatures) — negligible for long-lived gRPC streams, worth knowing on constrained links. Note `crypto/mldsa` is unavailable under the FIPS 140-3 Go module v1.0.0 (needs v1.26.0+), relevant only if `GOFIPS140` builds ever happen.
- Agent certs: 30-day lifetime, identity in URI SAN `polarbeam://agent/<uuid>`; server signs CSRs — private keys never leave the agent.
- Enrollment: `polarbeam-server token create --site nyc --network corp --ttl 24h` prints `<id>.<secret>` once (DB stores sha256, single-use; `--network` optional, default `default`, must already exist — networks are never auto-created) → `polarbeam-agent enroll --server host:443 --token …` writes cert + CA bundle. The agent inherits the token's network and never chooses it — no wire change, and the assignment is unforgeable by the enrollee.
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

**M7 — Path MTU probing (issue #23).** `path_mtu` probe type: agent prober (raw ICMP + DF probe mode, bounded search, black-hole detection), `PathMtuResult` proto, `path_mtu_current`/`path_mtu_events` + `mtuwatch`, probeadmin registration with int params, pair-detail API + UI card, docs.
*Verify:* direct + mesh path_mtu probes converge on the dev stack; a black hole (drop >1400 without ICMP errors via iptables) reports TIMEOUT with the flag, never OK; missing NET_RAW → ERROR naming the capability.

**M8 — Networks.** Named connectivity planes for shared sites (ISP management vs tenant networks): `networks` table + `network_id` on agents/tokens/meshes/direct probes with network-scoped expansion and re-derivation (PR ①), networks CRUD API/CLI + `--network` on token/mesh/probe (PR ②), read-path exposure (matrix sub-cells, `?network=` pair filter, per-(site, network) target sources) + SPA surfaces + docs (PR ③). No proto change — the network rides the join token server-side.
*Verify:* on the dev stack, a second network's mesh over the same sites yields disjoint series (the top-bar network filter shows each plane alone across every view, cross-plane pairs "not probed"); a single-network install renders pixel-identical with no filter control anywhere and unchanged `config_hash`es.

**M9 — Network-scoped tenant users.** Dashboard accounts an ISP can hand to a tenant: two network-scoped roles (`network_admin`/`network_viewer`) limited to an explicit set of networks, so one control plane carries a management plane plus per-tenant planes without any tenant seeing another. `user_networks` scope joined live onto the session, OIDC `role_rules`/`unmatched_role` mapping, and server-side filtering on every read path (PR ①); the `networkWrite` guard and the scoped write surface — probes, meshes, targets, tokens, thresholds — plus target ownership and the four-layer threshold merge (PR ②); the SPA capability object, settings-nav gating, and the docs (PR ③). Isolation is fail-closed by construction: `requireRole("admin")` stays an exact string compare, so every operator surface denies the scoped roles with no code of its own, today and for whatever is mounted behind it next, and a route-inventory test fails when a new route is not explicitly classified. Out-of-scope resources answer the same 404 as nonexistent ones, so a tenant cannot probe for another's topology. No proto, agent, or enrollment change — the plane still rides the join token.
*Verify:* a `network_admin` scoped to one tenant network signs in, sees only that plane's sites/agents/matrix/incidents, manages its own probes, meshes, targets, tokens, and thresholds, and finds no Users, Authentication, Banner, Networks, or Sites tab; a global admin's dashboard is unchanged, and an install with no scoped user behaves exactly as before.

## Dev environment

Dev compose = production compose (`deploy/compose/`: proxy + server + timescaledb) plus a dev overlay (`deploy/compose-dev/`): `agent-{nyc,lon,syd}` containers (a one-shot bootstrap service mints dev tokens and seeds a dashboard login `admin`/`polarbeam-dev`; `cap_add: NET_RAW`), optional netem profile (`delay 80ms loss 2%`) for realistic charts. Using the same base compose in dev exercises the SNI-passthrough path continuously. `make dev` runs the server locally + Vite proxy for SPA hot-reload; `make up` brings up the full containerized stack.

**Note:** `make up` must always include the dev overlay in dev environments — a prior lesson: restarting only the core compose silently drops overlay services (fake agents' tokens, monitoring). Makefile targets should be explicit about which files they compose.

## Verification (end-to-end)

Each milestone has its own gate above; the standing harness is `deploy/docker-compose.yml` — three agents meshed across fake "sites" with netem-induced latency/loss, exercised after every change (`make up && make seed`, then check matrix, pair drill-down, outage injection via iptables, spool replay via server stop/start).
