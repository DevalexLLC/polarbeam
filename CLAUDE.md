# PolarBEAM — project instructions

Central control plane + per-site Go agents measuring inter-site connectivity
(latency, loss, jitter, TCP/TLS timings, traceroute) with directional history.
Full design + milestone plan: `docs/architecture.md`.

## Hard constraints (user-mandated)

- **Air-gapped customer deployments.** Production sites can run PolarBEAM
  on isolated networks with zero INTERNET access (full local connectivity —
  probing is the product). Runtime binaries make no network calls beyond
  what an operator explicitly configures: probe targets (external ones are
  fine on connected sites) plus the optional default-off OIDC SSO, which
  calls the admin-configured IdP at login/test time (air-gapped sites leave
  it off; startup, local login, and probing never depend on it). Builds stay
  zero-fetch so every artifact is self-contained:
  `vendor/` is committed (`-mod=vendor` everywhere), generated protobuf code
  is committed (`internal/pb/`), the built SPA is committed (`web/dist/`).
  New Go deps: `make vendor` in the same change. Never add a build step that
  fetches anything — the dev/build environment IS online, but the
  `offline-build` CI gate and the air-gap bundle must keep working without it.
  The minimum Go version includes the security patch level in `go.mod`; keep
  both Go build stages and `docs/airgap-build.md` pinned to that exact version.
- **Control plane is containers-only** (proxy + server + TimescaleDB via
  compose). The nginx proxy does SNI passthrough on 443 — it never terminates
  TLS; agent mTLS is verified end-to-end in the Go server. It prepends a
  PROXY protocol v1 header on both routes; the server requires the header
  when `listen.proxy_protocol` is on (default off, shipped configs enable
  it) so rate limiting and enrollment see real client addresses.
- **The agent is a single static Go binary** (`CGO_ENABLED=0`), no runtime
  deps, shipped ONLY as a container image (`deploy/docker/agent.Dockerfile`,
  `--target release`; the default target is the dev overlay image). The
  release entrypoint runs `selfcheck` before `run` — the fail-loud preflight
  the retired systemd unit used to provide. No RPM/systemd packaging exists.
- **Fail loud.** Unknown YAML keys are fatal (`internal/strictyaml`),
  preflight names every problem, spool overflow is reported to the server,
  unsupported probe types report `UNSUPPORTED` instead of being skipped.

## Architecture invariants

- Agent identity = mTLS client cert URI SAN `polarbeam://agent/<uuid>`,
  never message fields. Direction (site A→B vs B→A) derives from cert + the
  server-side target row; it is unforgeable by construction.
- The DB is the sole certificate revocation authority (checked per-RPC +
  30 s stream sweep). No CRL/OCSP.
- Enrollment trust is explicit: `--ca-cert` or `--fingerprint` — no TOFU.
- The built-in CA signs agent client certs AND the auto-issued gRPC server
  cert (`listen.grpc_hostname` SAN). Operator TLS (`tls.*`) covers only the
  dashboard listener.
- Server streams FULL config snapshots keyed by `config_hash`; agents diff
  locally. All wire timings are int64 microseconds, -1 = not measured.
- Proto compatibility: fields are only added, never renumbered/repurposed
  (old agents in other languages must keep working).

## Workflows

- Build/test (offline): `make build`, `make test`. Dev stack: `make up` /
  `make down`, and `make reset` for teardown INCLUDING volumes (fresh
  DB/CA/tokens — what older notes call `down -v`; plain `make down -v`
  does NOT work, make eats `-v`). All three default
  `POLARBEAM_DB_PASSWORD`. ALWAYS composes base + dev overlay together;
  never `docker compose up` the base file alone (silently drops overlay
  services).
- Regenerate protos: `make proto` — buf + protoc-gen-go{,-grpc} are pinned
  in go.mod's `tool` block and run from `vendor/`, no host tooling — and
  commit the diff under `internal/pb/` (the `offline-build` CI job
  regenerates and rejects drift).
- Licensing/attribution: `LICENSE` (AGPL-3.0-only; Devalex LLC offers
  commercial exceptions, so external contributions need a CLA — see
  CONTRIBUTING.md) + hand-written `NOTICE` + generated `THIRD-PARTY-NOTICES`.
  The copyright/license line (year/holder/license) lives in FIVE editable
  places that must change together — `NOTICE`, `README.md`'s License
  section, the header in `tools/gen-third-party-notices.sh`, and the SPA's
  `views/About.tsx` + `views/Login.tsx` — plus TWO generated copies that
  must be regenerated after any change: the `THIRD-PARTY-NOTICES` header
  (`make notices`) and the committed `web/dist/` bundle (`make web`).
  `make notices` does NOT sync or validate them. All THREE files ship in
  every artifact — all three images (`/licenses`) and the air-gap bundle.
  Any new distribution channel must carry them too. `make notices`
  (`tools/gen-third-party-notices.sh`) regenerates the third-party file and
  is chained off BOTH `make vendor` and `make web`, because it folds four
  input groups: the Go stdlib (`$(go env GOROOT)/LICENSE`+`PATENTS` — it is
  linked in, and BSD-3 §2 covers binary redistribution), `vendor/`, the
  COMMITTED `web/THIRD-PARTY-LICENSES`, and `web/public/fonts/OFL.txt`. The
  SPA half is a separate generator (`web/tools/gen-spa-licenses.mjs`, run by
  `make web`) precisely because `node_modules` is gitignored and absent from
  the offline build — the top-level script must never need it. Both ABORT on
  a shipped component with no license file. TWO CI gates, because one cannot
  cover both halves: `offline-build` runs `make notices` before its existing
  clean-tree check, and `web-lint` (the only job with `node_modules`)
  regenerates `web/THIRD-PARTY-LICENSES` and diffs it — without the second,
  a react bump without `make web` would fold stale attribution and still
  pass. Neither adds a named check, so the ruleset is untouched.
- SPA style: `make web` runs `pnpm run lint && pnpm run fmt:check` BEFORE
  building, so a finding or an unformatted file blocks the dist rebuild;
  `make web-fix` = `oxlint --fix` + `oxfmt`. `make lint` is untouched
  (Go-only, offline — CONTRIBUTING.md promises that).
- Branding: the PolarBEAM mark uses the dashboard's neutral ink and indigo
  accent. `web/public/polarbeam-mark.svg` is the adaptive favicon; its static
  light/dark variants are used by the SPA and duplicated under `docs/assets/`
  for the README. Keep the geometry synchronized across all five source SVGs
  and rebuild `web/dist/` with `make web` after changing them.
- Dev host ports: proxy publishes on **9443** (443 is usually taken on dev
  boxes); inside the network agents use `proxy:443` as in production.
- Migrations: `internal/server/migrate/sql/NNNN_*.sql`, applied in filename
  order, one transaction each — except `NNNN_name.notx.sql` files, which run
  outside any transaction for DDL Postgres refuses in one (continuous
  aggregates) and must hold exactly ONE idempotent top-level statement (see
  `internal/server/migrate/migrate.go` package doc). Dev server auto-migrates;
  prod runs `polarbeam-server migrate` explicitly. **Once a migration has shipped in
  any release, it is immutable** — schema changes get a new numbered file
  (the `Pending` preflight tracks filenames, not content, so editing an
  applied file silently skips the change on upgrades). Pre-first-release,
  editing `0001_init.sql` is fine; recreate dev DBs with `down -v`.
- Conventional Commits (`feat(scope): ...`); see CONTRIBUTING.md.
- Branch → PR is ENFORCED, not convention: a GitHub ruleset protects `main`
  with no bypass actors, so even the repo owner cannot push to it. Work on a
  branch, open a PR, merge once CI is green (no approval required — a
  maintainer can self-merge). The six checks are required BY NAME
  (`offline-build`, `web-lint`, `db-test`, `docker-build (server)`,
  `docker-build (agent)`, `docker-build (proxy)` — the matrix legs carry an
  explicit `name:` for this), so renaming a CI job strands every PR on a
  check that no longer reports — update the ruleset in the same change.
  A separate `cla.yml` (pull_request_target, org-forked CLA Assistant at
  `DevalexLLC/cla-assistant-action`) blocks external PRs until the author
  signs `CLA.md`; signatures live on the unprotected `cla-signatures`
  branch. Never add a checkout of PR code to that workflow.
  Actions hygiene: every `uses:` is pinned to a full commit SHA (the repo
  setting requires it) with a trailing `# vX.Y.Z` comment; Dependabot bumps
  both together. Already committed to `main`
  locally? `git switch -c <branch>` (the commits follow), then
  `git branch -f main origin/main` to rewind main.
- `docs/install.md` is the zero-to-working-system operator guide: online image
  pulls and offline bundles, DNS/dashboard TLS/SNI proxy setup, explicit
  production migration and CA initialization, container agents,
  mandatory proxied-enrollment `--probe-address`, a baseline two-site mesh,
  direct-target examples, end-to-end verification, firewall rules,
  troubleshooting, lifecycle, upgrades, and backup scope. It uses the actual
  dashboard-user CLI (`user add --admin`) and makes clear that probe workloads
  are configured centrally rather than in agent YAML. `README.md` links it as
  the production installation entry point.
- `docs/probes.md` is the authoritative operator reference for supported probe
  transports, ports, parameters, assignment models, limitations, firewall
  requirements, scaling considerations, and large-infrastructure usage
  patterns. Keep its behavior descriptions in sync with `probeadmin`,
  `meshexpand`, and the agent prober implementations.
