# Security Policy

## Reporting a vulnerability

Report privately through GitHub:
**[Security → Report a vulnerability](https://github.com/DevalexLLC/polarbeam/security/advisories/new)**

Private vulnerability reporting is enabled on this repository, so your
report stays between you and the maintainers until a fix is available.

**Please do not open a public issue for a suspected vulnerability**, and
please do not disclose it publicly before a fix ships.

PolarBEAM is maintained by a small team. You will get an acknowledgement
as soon as we can manage, and we will keep you posted while we work on a
fix and a release. There is no bug bounty.

A report is much easier to act on when it includes:

- the version (`polarbeam-server version`, `polarbeam-agent version`) or
  the commit;
- the deployment shape — containerized control plane and agent,
  whether traffic passes the SNI proxy;
- the relevant configuration, with secrets redacted;
- what an attacker gains, and the steps to reproduce it.

## Supported versions

PolarBEAM is pre-1.0. Only the latest release receives security fixes;
there are no backports to earlier tags.

| Version | Supported |
| ------- | --------- |
| the most recent release | yes |
| every earlier tag | no |

## Scope

The areas where a flaw would matter most:

- **The built-in CA and enrollment** — agent client certificate issuance,
  the auto-issued gRPC server certificate, and the explicit enrollment
  trust anchors (`--ca-cert` / `--fingerprint`).
- **Agent identity and direction attribution** — identity comes from the
  mTLS client certificate's URI SAN, never from message fields, and the
  direction of a measurement derives from that certificate plus the
  server-side target row.
- **Certificate revocation** — revoked agents must lose access promptly.
- **Dashboard authentication** — password hashing, session cookies, CSRF
  enforcement, role checks on admin endpoints, and login rate limiting.
- **Result ingest** — an agent must not be able to write results for a
  probe or target that is not assigned to it.
- **Privilege boundaries on the agent host** — the agent container's
  capabilities, and file ownership under the agent's state volume.

Findings in third-party dependencies are welcome; please say which
vendored component and version is affected.

## Design decisions that are not vulnerabilities

These are deliberate and documented. Reports about them will be closed
with a pointer here, so please read this list first.

- **No CRL or OCSP.** The database is the sole revocation authority,
  checked on every RPC and by a 30-second sweep over live streams.
- **The reverse proxy never terminates TLS.** It is SNI passthrough on
  443; agent mTLS is verified end to end inside the Go server, so the
  proxy is deliberately not a trust boundary for identity. It is,
  however, the authoritative source of client addresses: it prepends a
  PROXY protocol header that the server honors only when
  `listen.proxy_protocol` is enabled. That is safe because the server's
  ports are reachable only on the private compose network; exposing
  8080/8443 directly while the knob is on would let anyone spoof client
  addresses — the install guide forbids exposing them.
- **The development stack ships known credentials.** The dev overlay
  seeds a dashboard login and self-signed certificates so `make up` works
  offline. It is for development only and must never be deployed;
  production setup is in `docs/install.md`.
- **The agent needs raw-socket access.** ICMP and traceroute probes
  require `CAP_NET_RAW`, or an unprivileged ICMP group range for the echo
  prober. Traceroute strictly requires a raw socket.
- **CI's `web-lint` job installs from the npm registry.** It lints SPA
  sources, which are dev-time inputs — the committed `web/dist/` is what
  ships, and no release path runs a package manager. Those installs go
  through pnpm under a 14-day minimum-release-age policy
  (`web/pnpm-workspace.yaml`): a freshly published package version cannot
  be resolved until it has been public for two weeks, which blunts
  fast-moving registry supply-chain attacks. The committed
  `pnpm-lock.yaml` is the trusted base, so lockfile diffs in PRs are
  security-relevant and must be reviewed as such. pnpm itself is pinned by
  version and integrity hash in `web/package.json`'s `packageManager`
  field.

  To be precise about the air-gap guarantee, because it is narrower than
  it sounds: `offline-build` proves the Go binaries compile and test with
  `GOPROXY=off` against vendored dependencies. It is not a claim that
  every build step is offline. Building the container images does need
  the network — base images, `apk add libcap` — which is why air-gapped
  sites consume the pre-built release bundle rather than rebuilding. `docs/airgap-build.md` lists
  exactly which steps touch the network. Supply-chain concerns in that
  packaging path are in scope; please report them.

  At runtime, the server performs outbound HTTP in exactly one optional,
  default-off feature: OIDC single sign-on. The calls happen at SSO login
  time once an admin has enabled a provider, and immediately when an
  admin uses the settings page's Test connection action (which works
  before enabling — it exists to prove connectivity). Local (break-glass)
  login and server startup never depend on that egress. The OIDC client
  secret is stored in the `oidc_settings` database table and is therefore
  part of database backups.

## Hardening guidance

Operator-facing security setup — TLS for the dashboard, firewall rules,
certificate rotation, and revocation — lives in `docs/install.md`.
