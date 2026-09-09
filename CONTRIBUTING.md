# Contributing to PolarBEAM

By participating in this project you agree to abide by the
[Code of Conduct](CODE_OF_CONDUCT.md).

## Licensing of contributions

PolarBEAM is licensed under AGPL-3.0-only, and Devalex LLC additionally
offers commercial licensing exceptions. That dual-licensing model only
works while Devalex LLC can relicense the whole codebase, so contributions
are accepted **only** under the project's contributor license agreement:

- **Individuals:** [CLA.md](CLA.md). You retain ownership of your
  contribution and grant Devalex LLC a broad license, including the right
  to relicense. Signing is automated — on your first pull request, a
  status check asks you to post the comment
  `I have read the CLA Document and I hereby sign the CLA`. You sign once;
  it covers all future contributions.
- **On behalf of an employer:** your employer additionally executes the
  [Entity CLA](CLA-ENTITY.md) by email.

A pull request cannot merge until its author has signed.

## Commits

Follow [Conventional Commits](https://www.conventionalcommits.org/):
`<type>(<scope>): <subject>` with types `feat`, `fix`, `docs`, `style`,
`refactor`, `perf`, `test`, `build`, `ci`, `chore`, `revert`.

- Subject: imperative mood, lowercase, no trailing period, ≤72 chars.
- Body: wrap at 72 chars, explain *why*, blank line after the subject.
- Reference issues in a trailer (`Refs: #123`), not the subject.

## Pull requests

`main` accepts no direct pushes — not from contributors, not from
maintainers. Every change lands through a pull request whose required
checks pass: `offline-build` (the air-gap gate), `web-lint`, and
`db-test`, plus `docker-build (server)` / `docker-build (agent)` /
`docker-build (proxy)`. CodeQL also reports advisory `Analyze` checks under
the current ruleset; review its alerts before merging. Reviews are not
required, so a maintainer can self-merge once CI is green; force-pushes and
branch deletion are blocked outright.

For pull requests from forks, GitHub Actions runs only after a maintainer
approves the workflow run — expect a short delay before CI starts on your
first PRs.

### Advisory image scans

CI's `image-scan` job scans the server, agent, and proxy images produced by
the successful `docker-build` matrix on every PR and push to `main`. Server
and agent use their `release` targets. Coverage is the existing
`linux/amd64` CI build; the release pipeline's `linux/arm64` images are not
scanned here. A failed build skips the scan job.

Open the workflow run's summary for per-image OS/library severity totals
and the number of findings with a fixed version available. Counts are
package/target occurrences, so one advisory may appear in several packages
or images. All severities and unfixed findings are included. **Unavailable**
means the scan failed or its report is missing/invalid; it never means zero
findings. The publication table reports download, conversion, and upload
failures separately. The `trivy-reports` artifact retains JSON and SARIF for
seven days, including when Security upload fails. Image-transfer artifacts
expire after one day; rerun all jobs if they have expired.

Under **Security → Code scanning**, filter Trivy results by category:
`trivy-server`, `trivy-agent`, or `trivy-proxy`. Findings alone do not fail
`image-scan`; scanner or reporting failures do fail that optional job.
GitHub can separately mark code-scanning checks red for new alerts. Both
remain advisory: neither the new job nor its SARIF checks is among the six
required checks above. Fork and Dependabot PRs use the normal
`pull_request` upload support, without privileged triggers or extra secrets.

Triage High/Critical findings first. When a supported base image supplies
the fixed OS package, update its pinned tag/digest and rebuild. A fixed
package version in the report does not guarantee a suitable base image is
published yet; otherwise track upstream and any applicable mitigation,
keeping unfixed findings visible. Go module and standard-library findings
belong here too. Track one issue per advisory/root cause across affected
images; use manual `govulncheck` for reachability follow-up rather than
opening duplicate triage items. There is no automated `govulncheck` job.

Trivy uses the version supplied by its SHA-pinned action, so Dependabot
action updates advance the scanner too. The three sequential scans share
one daily database cache, with normal freshness checks enabled. Scanner
and database downloads are CI-only; they must not enter Make targets or
air-gap bundles. The `offline-build` job still proves disconnected builds.

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
make bench              # DB-backed ingest benchmarks (needs POLARBEAM_TEST_DB_URL)
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
