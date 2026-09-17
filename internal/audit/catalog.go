package audit

// Event IDs. One constant per event type; docs/audit-logging.md lists the
// same catalog for operators, and a test in this package fails when a
// call site uses an ID that is not here (or a string literal instead of
// the constant).
const (
	// Dashboard authentication and sessions.
	EventLogin           = "auth.login"            // local password login attempt
	EventSSOStart        = "auth.sso.start"        // OIDC flow start refused
	EventSSOLogin        = "auth.sso.login"        // OIDC callback: login attempt
	EventSessionRejected = "auth.session.rejected" // request without a valid session or CSRF token
	EventSessionEnded    = "auth.session.ended"    // a session was destroyed (logout, expiry, revocation, disable)
	EventSessionRestored = "auth.session.restored" // a disabled user's live sessions became usable again

	// Federated account provisioning (JIT at OIDC login).
	EventSSOAccountCreated = "account.sso.created"
	EventSSOAccountUpdated = "account.sso.updated"

	// Every mutating dashboard request, and every refused read.
	EventAPIWrite    = "api.write"
	EventAuthzDenied = "authz.denied"

	// Agent credentials and sessions (gRPC).
	EventAgentAuth         = "agent.auth"          // mTLS identity refused
	EventAgentEnroll       = "agent.enroll"        // join-token enrollment
	EventAgentSessionStart = "agent.session.start" // config stream opened
	EventAgentSessionEnd   = "agent.session.end"   // config stream closed
	EventAgentCertRenew    = "agent.cert.renew"    // certificate renewal

	// Control-plane lifecycle.
	EventServerStart = "server.start"
	EventServerStop  = "server.stop"

	// Audit forwarding itself (the syslog forwarder's own transitions —
	// AU-5: the audit subsystem's failures are audited).
	EventForwardStart        = "audit.forward.start"
	EventForwardStop         = "audit.forward.stop"
	EventForwardDisconnected = "audit.forward.disconnected"
	EventForwardRecovered    = "audit.forward.recovered"
	EventForwardTest         = "audit.forward.test"

	// Settings writes that own a richer record than api.write.
	EventSyslogSettingsUpdate = "settings.syslog.update"

	// Operator CLI (polarbeam-server subcommands).
	EventCLIUserAdd       = "cli.user.add"
	EventCLITokenCreate   = "cli.token.create"
	EventCLICAInit        = "cli.ca.init"
	EventCLICARetire      = "cli.ca.retire"
	EventCLITLSInstall    = "cli.tls.install"
	EventCLISiteSet       = "cli.site.set"
	EventCLINetworkCreate = "cli.network.create"
	EventCLINetworkSet    = "cli.network.set"
	EventCLINetworkDelete = "cli.network.delete"
	EventCLITargetAdd     = "cli.target.add"
	EventCLITargetRemove  = "cli.target.rm"
	EventCLIProbeAdd      = "cli.probe.add"
	EventCLIProbeRemove   = "cli.probe.rm"
	EventCLIMeshCreate    = "cli.mesh.create"
	EventCLIMeshAdd       = "cli.mesh.add"
	EventCLIMeshRemove    = "cli.mesh.rm"
	EventCLIMeshDelete    = "cli.mesh.delete"
	EventCLIMigrate       = "cli.migrate"
)

// Def describes one catalogued event for the documentation generator and
// the completeness test.
type Def struct {
	// Description is the operator-facing one-liner.
	Description string
	// Attrs lists the event-specific attribute keys the event may carry
	// beyond the fixed ones (event, outcome, user, user_source, session,
	// remote).
	Attrs []string
}

// Catalog is every event the server can emit. Emit refuses IDs not here.
var Catalog = map[string]Def{
	EventLogin:           {"Local password sign-in attempt (success mints a session).", []string{"reason"}},
	EventSSOStart:        {"OIDC sign-in could not start (provider unavailable, disabled, or rate-limited).", []string{"reason"}},
	EventSSOLogin:        {"OIDC callback: federated sign-in attempt.", []string{"reason", "issuer"}},
	EventSessionRejected: {"Request refused for lack of a valid session cookie or CSRF token.", []string{"reason", "route", "path"}},
	EventSessionEnded:    {"A dashboard session ended.", []string{"reason", "observed_at"}},
	EventSessionRestored: {"A disabled account was re-enabled with live sessions.", []string{"reason"}},

	EventSSOAccountCreated: {"Federated account provisioned at first OIDC sign-in.", []string{"user_id", "user_target", "role", "networks", "issuer"}},
	EventSSOAccountUpdated: {"Federated account's username, role, or network scope changed at OIDC sign-in.", []string{"user_id", "user_target", "prev_username", "role", "prev_role", "networks", "prev_networks", "issuer"}},

	EventAPIWrite: {"A mutating dashboard request completed (any status).", []string{"route", "path", "status", "reason",
		"target", "mesh", "probe", "probe_type", "network", "site", "user_target", "user_id", "role", "networks", "disabled", "sessions_revoked", "ttl",
		"provider_changed", "policy_changed"}},
	EventAuthzDenied: {"A read was refused: wrong role, or a resource outside the caller's network scope.", []string{"route", "path", "status", "reason"}},

	EventAgentAuth:         {"An agent RPC presented no, a foreign, or a revoked client certificate.", []string{"reason"}},
	EventAgentEnroll:       {"Join-token enrollment attempt.", []string{"agent", "site", "network", "hostname", "probe_address", "reason"}},
	EventAgentSessionStart: {"An agent opened its config stream.", []string{"agent", "version"}},
	EventAgentSessionEnd:   {"An agent's config stream closed.", []string{"agent", "reason"}},
	EventAgentCertRenew:    {"Agent certificate renewal attempt.", []string{"agent", "not_after", "reason"}},

	EventServerStart: {"Control plane finished preflight and is listening, or failed preflight.", []string{"version", "grpc_addr", "http_addr", "proxy_protocol", "reason"}},
	EventServerStop:  {"Control plane is shutting down.", []string{"reason"}},

	EventForwardStart:        {"Syslog forwarding (re)started for a destination.", []string{"transport", "host", "port", "content", "on_failure"}},
	EventForwardStop:         {"Syslog forwarding stopped (the last record a process sends).", nil},
	EventForwardDisconnected: {"The collector became unreachable; records are being buffered.", []string{"host", "reason"}},
	EventForwardRecovered:    {"The collector is reachable again; sent after the buffered backlog.", []string{"outage", "dropped"}},
	EventForwardTest:         {"A test record sent by the settings page's Test button or the startup check.", nil},

	EventSyslogSettingsUpdate: {"The syslog forwarding settings were changed.", []string{"enabled", "transport", "host", "port", "content", "on_failure", "previous_enabled", "previous_host", "previous_port"}},

	EventCLIUserAdd:       {"Dashboard user created from the CLI.", []string{"user_target", "user_id", "role", "networks"}},
	EventCLITokenCreate:   {"Agent join token issued from the CLI.", []string{"site", "network", "ttl"}},
	EventCLICAInit:        {"Built-in CA created (or confirmed present).", []string{"algorithm", "fingerprint", "dir"}},
	EventCLICARetire:      {"Built-in CA directory moved aside.", []string{"dir", "retired_to"}},
	EventCLITLSInstall:    {"Dashboard TLS certificate and key installed.", []string{"cert_file", "key_file"}},
	EventCLISiteSet:       {"Site metadata updated from the CLI.", []string{"site"}},
	EventCLINetworkCreate: {"Network created from the CLI.", []string{"network"}},
	EventCLINetworkSet:    {"Network updated from the CLI.", []string{"network"}},
	EventCLINetworkDelete: {"Network deleted from the CLI.", []string{"network", "tokens_deleted"}},
	EventCLITargetAdd:     {"External target created or updated from the CLI.", []string{"target", "network"}},
	EventCLITargetRemove:  {"External target removed from the CLI.", []string{"target"}},
	EventCLIProbeAdd:      {"Probe assignment created from the CLI.", []string{"probe", "probe_type", "mesh", "site", "target", "network"}},
	EventCLIProbeRemove:   {"Probe assignment removed from the CLI.", []string{"probe"}},
	EventCLIMeshCreate:    {"Mesh group created from the CLI.", []string{"mesh", "network"}},
	EventCLIMeshAdd:       {"Site added to a mesh from the CLI.", []string{"mesh", "site"}},
	EventCLIMeshRemove:    {"Site removed from a mesh from the CLI.", []string{"mesh", "site"}},
	EventCLIMeshDelete:    {"Mesh group deleted from the CLI.", []string{"mesh", "probes_deleted"}},
	EventCLIMigrate:       {"Database migrations applied from the CLI.", []string{"reason"}},
}
