# Repository Guidelines

## Project Structure & Module Organization

`cmd/polarbeam-server/` and `cmd/polarbeam-agent/` contain executable entry
points. Core code lives under `internal/server/` and `internal/agent/`; tests
sit beside their packages. Protocol definitions are in
`proto/polarbeam/v1/`, with generated Go files committed under `internal/pb/`.
Dashboard source is in `web/src/`, with its committed build in `web/dist/`.
Deployment files live in `deploy/` (including the annotated
`deploy/agent/agent.example.yaml`), and design documentation in `docs/`. Go dependencies are committed in `vendor/`.

## Build, Test, and Development Commands

- `make build`: build static server and agent binaries into `bin/` offline.
- `make test`: run all Go tests with vendored dependencies.
- `make lint`: run `go vet` and `staticcheck` when installed.
- `make up` / `make down`: manage the base Compose stack and required dev
  overlay.
- `make web`: install pinned dependencies with pnpm (14-day
  minimum-release-age policy in `web/pnpm-workspace.yaml`), lint and
  format-check the SPA sources, then rebuild `web/dist/`.
- `make web-fix`: apply oxlint autofixes and reformat with oxfmt.
- `make proto`: regenerate protobuf and gRPC code in `internal/pb/`.

The Go version is pinned in `go.mod` and both Go builder images; keep those
pins and the air-gap prerequisites aligned.

CodeQL uses advanced setup with a manual Go build so it honors that pin; keep
default CodeQL setup disabled and do not replace the explicit build with
autobuild. Its checks are advisory under the current ruleset; review
reported alerts before merging.

After starting the stack, use `cd web && pnpm run dev` for Vite. Never run
the base Compose file alone for development.

Dashboard operational state is encoded in query parameters inside the hash
route and canonicalized by `web/src/routeState.ts`. New filters, pagination,
sorts, or row selections must use that module rather than component state or
local storage: discrete actions push history, debounced text input replaces
the current entry, and default or invalid values are omitted.

Pair and target uPlot charts share investigation-range behavior through
`web/src/components/Chart.tsx` and pure reconciliation in
`web/src/chartRange.ts`. Every caller must pass a context key containing all
resource, network, window, metric, and probe inputs that reset zoom.

`web/src/App.tsx` owns route-aware document titles; asynchronous target and
agent identities report back through its title callback. Initial route and
Settings-subsection failures use `web/src/components/PageError.tsx` (or its
Settings wrapper): keep raw causes in diagnostics, preserve URL state on
Retry, and expose Retry only when `web/src/pageState.ts` classifies the
failure as retryable.

Collection endpoints that use `readListQuery` share the parser and page
metadata in `internal/server/httpapi/listquery.go`. Those handlers must
declare every filter and sort allowlist there, preserve their legacy response
when query mode is off, narrow nonempty explicit networks through
`listQueryScope`, and perform search, stable tie-broken ordering, counting,
and pagination in SQL. `/api/v1/users` retains its established, separate
inventory contract.

Path-event query mode caps the newest 500 SQL matches before applying its
requested presentation sort and page. Keep its `time DESC, id DESC` index,
stable ID tie breakers, SQL-side changed-TTL calculation, capped `page.total`,
and `truncated` signal aligned; `window`-only requests retain the legacy JSON
shape and newest-500 behavior.

## Coding Style & Naming Conventions

Format Go with `gofmt`; use tabs, lowercase packages, and conventional
`MixedCaps` names. Return explicit errors rather than silent fallbacks.
TypeScript is strict: use two-space indentation, single quotes, semicolon-free
style, PascalCase components, and camelCase functions and variables — oxfmt
and oxlint enforce this from `web/.oxfmtrc.json` and `web/.oxlintrc.json`, so
run `make web-fix` rather than hand-matching the style. Preserve
protobuf field numbers and regenerate committed artifacts after source changes.

## Testing Guidelines

Write Go tests as `*_test.go` using `TestXxx` names and table-driven cases where
appropriate. Run focused tests with
`go test -mod=vendor ./internal/server/httpapi -run TestName`, then run
`make test` before submission. Dependency-free SPA unit and contract tests run
with `cd web && pnpm test` and are included in `make web`. No numeric coverage
threshold exists; cover new behavior, failure paths, and regressions. UI
changes must pass `make web`.

## Commit & Pull Request Guidelines

Use Conventional Commits, for example `fix(store): reject stale results`.
Subjects are imperative, lowercase, no more than 72 characters, and have no
trailing period. Wrap bodies at 72 characters and explain why the change is
needed. Reference issues in trailers such as `Refs: #123`.

`main` takes no direct pushes — a GitHub ruleset with no bypass actors covers
maintainers too. Branch, open a PR, and merge once all six required checks
pass; no approval is needed.

PRs should describe the behavior change, link relevant issues, and list exact
verification commands. Bug fixes must include observed broken and fixed
outputs. Include screenshots for visible dashboard changes and commit updated
`web/dist/`, `internal/pb/`, or `vendor/` artifacts with their sources.

## Security & Configuration

Runtime binaries must have no external dependencies, and no network access
beyond what an operator explicitly configures. The one sanctioned exception is
the optional, default-off OIDC single sign-on: the server calls the
admin-configured identity provider at SSO login time and when an admin runs
the settings Test connection action (see `docs/install.md`); startup, local
login, and probing never depend on it.
Package every required artifact so installation and operation remain fully
disconnected in air-gapped environments — such sites simply leave SSO
disabled. Treat shipped SQL migrations as immutable and never reuse
development credentials in production.
