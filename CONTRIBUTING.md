# Contributing to PolarBEAM

## Licensing of contributions

PolarBEAM is licensed under AGPL-3.0-only, and Devalex LLC additionally
offers commercial licensing exceptions. That dual-licensing model only
works while Devalex LLC holds the full copyright, so external
contributions require a signed contributor license agreement (CLA)
assigning or broadly licensing the contribution to Devalex LLC before a
pull request can merge. Open an issue to request the CLA text.

## Commits

Follow [Conventional Commits](https://www.conventionalcommits.org/):
`<type>(<scope>): <subject>` with types `feat`, `fix`, `docs`, `style`,
`refactor`, `perf`, `test`, `build`, `ci`, `chore`, `revert`.

- Subject: imperative mood, lowercase, no trailing period, ≤72 chars.
- Body: wrap at 72 chars, explain *why*, blank line after the subject.
- Reference issues in a trailer (`Refs: #123`), not the subject.

## Pull requests

`main` accepts no direct pushes — not from contributors, not from
maintainers. Every change lands through a pull request whose checks pass:
`offline-build` (the air-gap gate), `web-lint`, and one `docker-build` per
image. Reviews are not required, so a maintainer can self-merge once CI is
green; force-pushes and branch deletion are blocked outright.

## Ground rules

- **Builds must work offline.** Never add a build step that reaches the
  network. New Go dependencies are vendored (`make vendor`) in the same
  change. Generated protobuf code (`internal/pb/`) and the built SPA
  (`web/dist/`) are committed; regenerate with `make proto` / `make web`
  and include the diff.
- **Fail loud.** Unknown config keys are fatal. Dependencies are checked at
  startup preflight and failures name the problem. No silent fallbacks,
  no repurposed environment variables.
- **Every change is verifiable.** State how you verified it in the PR:
  the command, the broken output (if fixing), the fixed output.

## Development

```
make build test lint    # offline
make up                 # dev stack (compose base + dev overlay together)
make web                # rebuild the SPA: lints, format-checks, then builds
make web-fix            # apply oxlint autofixes and reformat
```

`make up` always composes the base stack *and* the dev overlay. Do not
`docker compose up` the base file alone in a dev environment — it silently
removes the overlay services (fake agents, their tokens, monitoring).

SPA style is enforced, not conventional: `make web` fails on any oxlint
finding or unformatted file before it rebuilds `web/dist/`, and CI's
`web-lint` job repeats both checks. That job installs from the npm registry
— it gates dev tooling, so it sits outside the offline guarantee that
`offline-build` enforces for everything shipped.

The SPA uses pnpm, pinned with an integrity hash in `web/package.json`'s
`packageManager` field. One-time setup on Node 24: `corepack enable pnpm`
(corepack ships with Node 24; from Node 25 it needs `npm i -g corepack`
first). `web/pnpm-workspace.yaml` enforces a supply-chain policy: no
package version younger than 14 days is ever resolved
(`minimumReleaseAge`), strictly — a too-young resolution fails loudly
instead of falling back. The committed `pnpm-lock.yaml` is the trusted
base, which makes lockfile diffs in PRs security-relevant: review them.
