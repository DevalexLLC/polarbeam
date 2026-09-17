# Audit logging

PolarBEAM's control plane records every security-relevant action as an
**audit record**: a structured log line whose first field is `event=`, the
record's type. This document is the operator reference for those records:
what triggers each one, which fields it carries, and how the set maps onto
the audit controls federal and DoD deployments are assessed against (NIST
SP 800-53 Rev. 5 AU family, the DISA Application Security and Development
STIG, NIST SP 800-171 / CMMC Level 2, OMB M-21-31).

Audit records are emitted by `polarbeam-server` — the dashboard API, the
agent-facing gRPC API, the server lifecycle, and every state-changing
`polarbeam-server` CLI subcommand. Agents do not emit audit records; their
credential decisions are recorded by the server that made them.

Today the records go to the server's standard error (`docker compose logs
server`) alongside operational messages. Forwarding them to a syslog
collector is a separate feature; this catalog is the contract it forwards.

## Record layout

Every audit record is one line in the server log:

```
2026-09-17T14:03:22.418Z level=INFO msg="agent enrolled" event=agent.enroll outcome=success user=8b2f... user_source=agent remote=203.0.113.9 agent=8b2f... site=1c3e... hostname=edge-nyc probe_address=10.10.0.5
```

The fixed fields come first, in this order, and answer the questions NIST
AU-3 asks of every record:

| Field | AU-3 element | Meaning |
|---|---|---|
| `event` | what type of event | The record's type, from the catalog below. Absent on operational (non-audit) messages, so the presence of `event=` is what marks a line as an audit record. |
| timestamp (line prefix) | when | UTC, from the server clock, millisecond precision on stderr (the syslog forwarder emits microseconds). An expired session is recorded at its expiry time, with `observed_at` carrying the cleanup time. |
| `outcome` | outcome | `success`, `failure` (attempted and did not complete), or `denied` (refused before it ran: no session, wrong role, out-of-scope resource, rate limit, revoked certificate). |
| `user` | identity of the subject | The dashboard username, the invoking OS user for the CLI, or the agent UUID. For a failed login it is the username that was *claimed*. |
| `user_source` | identity of the subject | `local`, `oidc`, `cli`, or `agent`. |
| `session` | identity of the subject | The dashboard session's row UUID. Never the session token. Every request, the login that minted the session, and the record that ended it carry the same value, so a session's whole life is one join key. |
| `remote` | source | The client's IP address as the server saw it. Behind the SNI proxy this is the real client only when `listen.proxy_protocol` is on (the shipped compose configs enable it). |
| `level` | — | `INFO` for `outcome=success`, `WARN` for `failure` and `denied`. **`log.level` must be `info` (the default) or `debug` where audit records are required**: `warn` keeps only the refusals and failures and silently drops every successful login, configuration change, session end, and CLI action. |

Event-specific fields follow. Two are shared by the dashboard records:
`route` is the HTTP method and mux pattern (`DELETE /api/v1/config/targets/{name}`),
the record's sub-type; `path` is the concrete path, which carries the ids
and names of the objects PUT and DELETE routes act on. `status` is the HTTP
status the request received.

**Nothing secret is ever logged.** Passwords, session tokens, CSRF tokens,
join tokens, OIDC client secrets, PEM keys, and cookies are refused by the
audit layer itself (an attribute under any such key is dropped and an error
is logged), not merely omitted by convention.

## Event catalog

Every event the server can emit. The `internal/audit` package carries the
same table in code, and a test fails when the two disagree or when a call
site names an event that is not here.

### Dashboard authentication and sessions

| Event | Trigger | Extra fields |
|---|---|---|
| `auth.login` | Local password sign-in attempt. Success mints a session (`session` set). Failure carries `reason`: `invalid_credentials` (unknown user, wrong password, or a federated account trying a password), `disabled`, `password_changed` (rotated mid-request), or `rate_limited` (denied). | `reason` |
| `auth.sso.start` | OIDC sign-in could not start: `rate_limited` (denied), `provider_unavailable`, `disabled`. | `reason` |
| `auth.sso.login` | OIDC callback. Success carries `session` and `issuer`; a failure carries the flow step that failed (`state_mismatch`, `code_exchange`, `settings_changed_during_login`, …); `user_is_disabled` and a policy denial are `denied`. Once the identity provider has asserted an identity, `user` is that username even when no session was minted. | `reason`, `issuer` |
| `auth.session.rejected` | A request carried no usable session (`no_session_cookie`, `unknown_or_expired_session`) or a mutating request lacked the CSRF token (`csrf`). | `reason`, `route`, `path` |
| `auth.session.ended` | A session ended: `logout`; `expired` (recorded at the expiry time, `observed_at` = when the cleanup ran); `password_changed`; `password_reset`; `user_deleted`; `oidc_policy_changed` (every federated user's session, each named); `user_disabled` (the account's live sessions, which the disable hides). | `reason`, `observed_at` |
| `auth.session.restored` | A disabled account was re-enabled while it still had unexpired sessions; each becomes usable again (`user_enabled`). | `reason` |

### Federated accounts

| Event | Trigger | Extra fields |
|---|---|---|
| `account.sso.created` | An OIDC user signed in for the first time and an account was provisioned. Recorded once the account write commits, whether or not the sign-in then completes. | `user_id`, `user_target`, `role`, `networks`, `issuer` |
| `account.sso.updated` | An OIDC sign-in changed the account's username, role, or network scope (the provider's assertion is applied on every sign-in). `user_id` is the stable account UUID that links a renamed identity to its history. | `user_id`, `user_target`, `prev_username`, `role`, `prev_role`, `networks`, `prev_networks`, `issuer` |

### Dashboard requests

| Event | Trigger | Extra fields |
|---|---|---|
| `api.write` | Every mutating dashboard request (POST, PUT, DELETE) behind a session, whatever its outcome — one record per request. `route` is the sub-type; `path` names the object for PUT/DELETE; creates add the object they made (`target`, `mesh`, `probe` + `probe_type`, `network`, `site`, `user_target` + `user_id` + `role` + `networks`; a join token adds `site`, `network`, `ttl` and never the token). Account mutations add `user_target`, `disabled`, `networks`, `sessions_revoked`; the OIDC settings write adds `enabled`, `provider_changed`, `policy_changed`, `sessions_revoked`. Logout is the one write with its own record (`auth.session.ended`). | `route`, `path`, `status`, `reason`, and the per-object fields above |
| `authz.denied` | A read was refused: wrong role (`status` 403), or a resource outside the caller's network scope. Successful reads are deliberately not recorded — this is an audit trail, not an access log. | `route`, `path`, `status`, `reason` |

**Scope denials.** A network-scoped role asking for another tenant's
resource receives a 404 that is byte-identical to a nonexistent one (the
response must not let tenants enumerate each other). The audit record is
not so coy: `reason=out_of_scope` when the server checked the scope
directly, `reason=not_found_or_out_of_scope` when the lookup folded scope
into the query and the two cases are genuinely indistinguishable, and
`reason=not_found` for a plain miss on a scope-blind lookup. The first two
are `outcome=denied`; the last is `failure`.

### Agents (gRPC)

| Event | Trigger | Extra fields |
|---|---|---|
| `agent.auth` | An agent RPC was refused at the identity check: `no_peer`, `no_client_certificate`, `not_agent_certificate`, or `revoked_or_unknown` (the database is the revocation authority). Always `denied`. | `reason`, `agent` |
| `agent.enroll` | Join-token enrollment. Success (`agent enrolled`) names the new agent, its site, the hostname it claimed, and the probe address recorded. `invalid_or_used_token` is `denied`; `missing_token_or_csr`, `csr_rejected`, `internal` are failures. | `agent`, `site`, `hostname`, `probe_address`, `reason` |
| `agent.session.start` | An authenticated agent opened its config stream (`agent connected`). | `agent`, `version` |
| `agent.session.end` | The stream closed (`agent disconnected`): `closed` (the agent went away, success); `revoked` (the periodic re-check found the certificate revoked, denied); `config_unavailable`, `send_failed`, `unconfirmable` (failures). | `agent`, `reason` |
| `agent.cert.renew` | Certificate renewal. Success carries the new `not_after`; `revoked_or_unknown` is denied; `csr_rejected` and `internal` are failures. | `agent`, `not_after`, `reason` |

### Control-plane lifecycle

| Event | Trigger | Extra fields |
|---|---|---|
| `server.start` | Both listeners are up (success; audit begins at startup), or preflight failed and the server never started (`failure`, `reason=preflight`; the cause is on the preceding `error:` line). | `version`, `grpc_addr`, `http_addr`, `proxy_protocol`, `reason` |
| `server.stop` | The server is shutting down: `signal` (success), `listener_error` (failure). | `reason` |

### Operator CLI

Each state-changing `polarbeam-server` subcommand records what it did once
it succeeded. `user` is the invoking `$USER` (or `unknown` inside the
release image, which sets none) with `user_source=cli`. The CLI runs in
its own process, so its records go to that process's standard error.

| Event | Subcommand | Extra fields |
|---|---|---|
| `cli.user.add` | `user add` | `user_target`, `user_id`, `role`, `networks` |
| `cli.token.create` | `token create` (the token itself is never logged) | `site`, `network`, `ttl` |
| `cli.ca.init` | `ca init` | `dir`, `algorithm`, `fingerprint` |
| `cli.ca.retire` | `ca retire` | `dir`, `retired_to` |
| `cli.tls.install` | `tls install` | `cert_file`, `key_file` |
| `cli.site.set` | `site set` | `site` |
| `cli.network.create` / `cli.network.set` / `cli.network.delete` | `network …` | `network`, `tokens_deleted` |
| `cli.target.add` / `cli.target.rm` | `target …` | `target`, `network` |
| `cli.probe.add` / `cli.probe.rm` | `probe …` | `probe`, `probe_type`, `mesh`, `site`, `target`, `network` |
| `cli.mesh.create` / `cli.mesh.add` / `cli.mesh.rm` / `cli.mesh.delete` | `mesh …` | `mesh`, `site`, `network`, `probes_deleted` |
| `cli.migrate` | `migrate` (success, or `failure` with `reason=apply_failed`) | `reason` |

## Standards mapping

How the catalog satisfies the audit-content and audit-event requirements
an application is assessed against. Control text is abridged; the cited
identifiers are the ones an assessor will ask for.

| Requirement | Where it is met |
|---|---|
| **NIST SP 800-53 AU-2** Event logging — identify and log the event types (logons, privilege use, account and attribute changes, configuration changes). | The catalog above. Every mutating dashboard request and CLI subcommand is an event; logons, denials, and credential decisions are their own events. |
| **AU-3** Content of audit records — type, when, where, source, outcome, identity. | The fixed fields (`event`, timestamp, `outcome`, `user`/`user_source`/`session`, `remote`); `route`/`path` name the component and object. |
| **AU-3(1)** Additional audit information. | The per-event extra fields (previous role and scope on a federated update, sessions revoked by a change, the reason behind every refusal). |
| **AU-8** Time stamps — UTC or with offset, defined granularity. | UTC from the server clock; milliseconds on stderr, microseconds when forwarded. Expiries are stamped when access actually ended. |
| **AU-12** Audit record generation for the AU-2 events with the AU-3 content. | The `internal/audit` package is the single emission path; a test fails when a call site uses an uncatalogued event. |
| **AC-7 / ASD STIG V-222462** Log successful and unsuccessful logon attempts. | `auth.login`, `auth.sso.login` with `reason`; rate limiting is recorded as `denied`. |
| **ASD STIG V-222413/414/415/416/421/467** Audit account creation, modification, disabling, enabling, removal. | `api.write` on the `/users` routes with `user_target`, `disabled`, `networks`, `sessions_revoked`; `account.sso.created`/`updated` for federated accounts; `cli.user.add`. |
| **V-222441/442/443/445/464** Audit session creation, destruction, timeouts, and the start and end of user access. | `auth.login` (creation, with `session`), `auth.session.ended` with `reason` (logout, expired, revoked, disabled), `auth.session.restored`; `agent.session.start`/`end` for agents. |
| **V-222446/473/498/499** Time stamp on every record, mappable to UTC, one-second granularity or better. | Timestamp prefix, UTC, sub-second. |
| **V-222448/470** Connecting system IP address. | `remote`; the agent's `probe_address` on enrollment. |
| **V-222449/477** Username or user id, identity of the process. | `user`, `user_source`, `session`, `user_id` on account records, `agent` on agent records. |
| **V-222474** Which component, feature, or function triggered the event. | `event` (the type) and `route` (the API surface), or the `cli.*` prefix. |
| **V-222476** Outcome of the event. | `outcome` on every record. |
| **V-222450/454/458/463/478** Privileged activity and privilege grants/denials. | Role and scope on account records; `authz.denied` for refused privilege; every CLI subcommand is privileged activity and is recorded with the invoking user. |
| **V-222512** Audit who makes configuration changes. | Every `api.write` and `cli.*` record carries the actor. |
| **V-222468/469** Initiate auditing at startup; log shutdown. | `server.start`, `server.stop`. |
| **V-222444** Do not write sensitive data into logs. | Enforced by the audit layer's key fence (see Record layout). |
| **NIST SP 800-171 3.3.1 / 3.3.2, CMMC AU.L2-3.3.1 / 3.3.2** Create audit records; trace actions to individual users. | The catalog; `user` + `session` on every dashboard record, `user_id` for renamed federated identities. |
| **OMB M-21-31** Key-value formatting, a unique identifier per event type, source IP, username where appropriate. | Every field is `key=value`; `event` is the unique type identifier; `remote`, `user`. |

## Known gaps

- Certificate **revocation** has no CLI or API surface yet (it is a direct
  database update), so it produces no audit record of its own; the
  consequence is recorded (`agent.auth` denied, `agent.session.end`
  reason `revoked`).
- Successful **reads** are not recorded (no access log). `authz.denied`
  covers refused reads.
- Records reach standard error only. Off-loading to a central collector
  (AU-4(1), V-222481/482) is the syslog forwarding feature, documented
  separately when it ships.
