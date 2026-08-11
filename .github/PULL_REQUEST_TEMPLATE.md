<!--
  Title must follow Conventional Commits: <type>(<scope>): <subject>
  e.g. fix(agent): handle spool overflow during reconnect
-->

## What & why

<!-- What does this change, and why? Link issues with "Refs: #123". -->

## How I verified it

<!-- Required (see CONTRIBUTING.md): the command you ran, the broken output
     if this fixes something, and the fixed/passing output. -->

## Checklist

- [ ] `make build test lint` passes locally (offline — no build step reaches
      the network)
- [ ] New Go dependencies are vendored (`make vendor`) in this PR; generated
      code (`internal/pb/`, `web/dist/`) is regenerated and committed if
      affected
- [ ] SPA changes: `make web` passes (lint + format gate before the dist
      rebuild)
- [ ] I have read [CLA.md](../CLA.md) and will sign via the CLA check on this
      PR (first-time contributors; see CLA-ENTITY.md if contributing for an
      employer)
