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

After starting the stack, use `cd web && pnpm run dev` for Vite. Never run
the base Compose file alone for development.

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
`make test` before submission. No numeric coverage threshold exists; cover new
behavior, failure paths, and regressions. UI changes must pass `make web`.

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
