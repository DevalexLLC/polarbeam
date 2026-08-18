// Shapes returned by /api/v1/* (the contract implemented by
// internal/server/httpapi).

export interface User {
  username: string
  role: 'admin' | 'viewer'
  // Federated accounts have no password here — their credential lives at
  // the IdP — so the UI hides password management for them.
  auth_source: 'local' | 'oidc'
}

export interface LoginResponse {
  user: User
  csrf_token: string
  // The server's own build, for the About page. Carried on login and on
  // auth/me, never on the unauthenticated auth/providers.
  version: string
}

export interface Site {
  name: string
  display_name: string
  location: string
  // null until an operator places the site (`polarbeam-server site set`);
  // 0 is a real coordinate, so never truthiness-check these.
  latitude: number | null
  longitude: number | null
}

export interface SitesResponse {
  sites: Site[]
}

export interface AgentInfo {
  id: string
  site: string
  hostname: string
  probe_address: string
  version: string
  last_seen_at: string | null
  enrolled_at: string
  config_hash: string
  cert_not_after: string | null
  cert_revoked_at: string | null
  offline: boolean
  probes_failing: number
  probes_total: number
  dropped_results: number
  last_dropped_at: string | null
}

export interface AgentsResponse {
  agents: AgentInfo[]
}

// GET /api/v1/agents/health — 24h of per-agent probe success counts in
// 30-min buckets. Buckets are sparse and agents with no results in the
// window are absent: join on id against /api/v1/agents and render missing
// data honestly (never an invented 100%).
export interface AgentHealthBucket {
  t: number // bucket start, UTC epoch seconds
  samples: number
  ok: number
}

export interface AgentHealth {
  id: string
  buckets: AgentHealthBucket[]
}

export interface AgentHealthResponse {
  window: string
  bucket_s: number
  agents: AgentHealth[]
}

// GET /api/v1/agents/{id}/health — one agent's per-probe 24h buckets. The
// series list comes from series_state, so a configured-but-silent series
// appears with empty buckets. target is the external target's name and is
// null for agent-kind targets, which are labeled by dst_site instead;
// target_kind is '' when the target row is gone.
export interface AgentProbeHealth {
  probe_id: string
  type: string
  target_kind: 'agent' | 'external' | ''
  target: string | null
  dst_site: string | null
  last_status: string
  last_time: string
  failing: boolean
  open_since: string | null
  error: string | null
  buckets: AgentHealthBucket[]
}

export interface AgentProbeHealthResponse {
  window: string
  bucket_s: number
  probes: AgentProbeHealth[]
}

// GET /api/v1/agents/{id}/health/bucket?t=… — the failure breakdown behind
// one 30-min health-strip slot. Without probe_id, traceroute is excluded so
// counts reconcile with the fleet strip; with probe_id they reconcile with
// that probe's own strip. groups is [] for an all-OK (or aged-out) bucket.
// target/dst_site semantics match AgentProbeHealth.
export interface AgentBucketFailureGroup {
  probe_id: string
  type: string
  target_kind: 'agent' | 'external' | ''
  target: string | null
  dst_site: string | null
  status: string
  count: number
  last_error: string | null // ≤128 chars, truncated at ingest
  last_time: string
}

export interface AgentBucketFailuresResponse {
  t: number
  bucket_s: number
  groups: AgentBucketFailureGroup[]
}

export type CellStatus = 'ok' | 'degraded' | 'down' | 'stale'

export interface MatrixProbe {
  type: string
  status: string
  latency_us: number | null
  latency_source: string
  loss_pct: number | null
  as_of: string
}

export interface MatrixCell {
  src: string
  dst: string
  status: CellStatus
  latency_us: number | null
  latency_source: string
  loss_pct: number | null
  as_of: string
  probes: MatrixProbe[]
}

export interface MatrixResponse {
  sites: Site[]
  cells: MatrixCell[]
  horizon_s: number
}

// Shared dashboard thresholds (GET/PUT /api/v1/settings). Latency is wire
// microseconds; the settings form converts to ms at the edge.
export interface ThresholdSettings {
  latency_warn_us: number
  latency_crit_us: number
  loss_warn_pct: number
  loss_crit_pct: number
}

// One per-site-pair override row: the unordered pair a/b (lexically
// sorted by the server), each field null = inherit the global value.
export interface PathThresholdOverride {
  a: string
  b: string
  latency_warn_us: number | null
  latency_crit_us: number | null
  loss_warn_pct: number | null
  loss_crit_pct: number | null
  updated_at: string
  updated_by: string
}

export interface SettingsResponse {
  thresholds: ThresholdSettings
  overrides: PathThresholdOverride[]
  updated_at: string
  updated_by: string
  // Advisory, PUT only: overrides whose effective values the new globals
  // left inconsistent (never blocks the write).
  warnings?: string[]
}

// Which table served a windowed query. Raw windows (24h/7d) have no
// percentile fields; hourly/daily (30d+) do.
export type SeriesSource = 'raw' | 'hourly' | 'daily'

export interface LatencySummary {
  min_us: number | null
  avg_us: number | null
  max_us: number | null
  // Present only when source != 'raw' (server omits the keys otherwise).
  p50_us?: number | null
  p95_us?: number | null
  p99_us?: number | null
}

export interface DirectionSummary {
  status: CellStatus
  last_ok_at: string | null
  latency: LatencySummary
  latency_source: string
  loss_pct: number | null
  samples: number
  jitter_avg_us: number | null
  tcp_connect_avg_us: number | null
  tls_handshake_avg_us: number | null
  checks: MatrixProbe[]
}

export interface PairResponse {
  a: string
  b: string
  window: string
  source: SeriesSource
  a_to_b: DirectionSummary
  b_to_a: DirectionSummary
}

export interface SeriesPoint {
  t: number // UTC epoch seconds (bucket start)
  min_us: number | null
  avg_us: number | null
  max_us: number | null
  loss_pct: number | null
  samples: number
  failures: number
  // Present only when source != 'raw' (server omits the keys otherwise).
  p50_us?: number | null
  p95_us?: number | null
  p99_us?: number | null
}

export interface SeriesResponse {
  metric: 'latency' | 'loss'
  window: string
  resolution_s: number
  source: SeriesSource
  latency_source: string
  a_to_b: { latency_source: string; points: SeriesPoint[] }
  b_to_a: { latency_source: string; points: SeriesPoint[] }
}

export interface OutageEvent {
  id: string
  kind: 'probe_failing' | 'agent_offline' | 'probe_degraded'
  agent: string
  src_site: string
  dst_site: string | null
  target: string | null
  probe_type: string | null
  opened_at: string
  closed_at: string | null
  error: string | null
}

export interface OutagesResponse {
  window: string
  // Server clock at response time; the incident timeline anchors its
  // bucket grid here so a skewed browser clock cannot shift the chart
  // away from the server-evaluated window.
  now: string
  outages: OutageEvent[]
}

export interface Hop {
  ttl: number
  addrs: string[]
  rtt_us: number[]
}

export interface PathEvent {
  id: string
  time: string
  agent: string
  src_site: string
  dst_site: string | null
  target: string | null
  old_path_hash: string
  new_path_hash: string
  old_hops: Hop[]
  new_hops: Hop[]
}

export interface PathEventsResponse {
  window: string
  events: PathEvent[]
}

export interface CurrentPath {
  // (agent_id, probe_id) is the series identity: several destination
  // agents or templates give one source hostname several probe IDs,
  // while several source agents at one site share a probe ID.
  agent_id: string
  probe_id: string
  agent: string
  updated_at: string
  dest_reached: boolean
  path_hash: string
  hops: Hop[]
}

export interface TracerouteResponse {
  a: string
  b: string
  a_to_b: { paths: CurrentPath[] }
  b_to_a: { paths: CurrentPath[] }
}

// GET /api/v1/path-mtu/{a}/{b} — latest usable path MTU measurement per
// direction. Sizes are IP-packet bytes including the IP header.
export interface CurrentPathMtu {
  // (agent_id, probe_id) is the series identity: several destination
  // agents or templates give one source hostname several probe IDs,
  // while several source agents at one site share a probe ID.
  agent_id: string
  probe_id: string
  agent: string
  updated_at: string
  largest_ok_bytes: number
  smallest_failed_bytes: number
  next_hop_mtu_bytes: number
  ip_version: number
  black_hole: boolean
  local_constraint: boolean
  rtt_us: number | null
}

export interface PathMtuResponse {
  a: string
  b: string
  a_to_b: { mtus: CurrentPathMtu[] }
  b_to_a: { mtus: CurrentPathMtu[] }
}

export const WINDOWS = ['24h', '7d', '30d', '90d', '365d'] as const
export type Window = (typeof WINDOWS)[number]

// --- Probe-workload config (/api/v1/config/*). Reads any session; writes
// admin-only. Cadence fields are wire integer milliseconds; the probes form
// converts to seconds at the edge like the thresholds form does µs↔ms.

export const PROBE_TYPES = ['icmp', 'tcp', 'tls', 'http', 'dns', 'ntp', 'traceroute', 'path_mtu'] as const
export type ProbeTypeName = (typeof PROBE_TYPES)[number]

export interface TargetConfig {
  id: string
  kind: 'agent' | 'external'
  name: string
  address?: string
  port?: number
  url?: string
  probe_count: number
  created_at: string
}

export interface TargetsConfigResponse {
  targets: TargetConfig[]
}

export interface MeshConfig {
  id: string
  name: string
  sites: string[]
  probe_count: number
}

export interface MeshesConfigResponse {
  meshes: MeshConfig[]
}

export interface SiteConfig {
  id: string
  name: string
  display_name: string
  location: string
  // null = unplaced; 0 is a real coordinate, never truthiness-check.
  latitude: number | null
  longitude: number | null
  created_at: string
  agent_count: number
  mesh_count: number
  probe_count: number
}

export interface SitesConfigResponse {
  sites: SiteConfig[]
}

// Join tokens (/api/v1/config/tokens) are admin-only including the list.
// Status (active / expired / used) is derived client-side from these fields.
export interface JoinToken {
  id: string
  site: string
  created_by: string
  created_at: string
  expires_at: string
  used_at: string | null
  used_by_agent: string | null
  used_by_hostname: string | null
}

export interface TokensResponse {
  tokens: JoinToken[]
}

// The cleartext token exists only in this response — shown once, never
// recoverable.
export interface TokenCreateResponse {
  token: string
  site: string
  expires_at: string
}

export interface ProbeConfig {
  id: string
  site?: string
  target?: string
  mesh?: string
  type: string
  interval_ms: number
  timeout_ms: number
  train_count: number
  train_spacing_ms: number
  params: Record<string, string>
  enabled: boolean
  created_at: string
  updated_at: string
  updated_by?: string
}

export interface ProbesConfigResponse {
  probes: ProbeConfig[]
}

// GET /api/v1/config/probe-types — the server-side param registry. The
// probe form renders its param fields from this, so the set of accepted
// keys has exactly one source of truth (internal/server/probeadmin).
export interface ParamSpec {
  key: string
  hint: string
  kind: 'string' | 'port' | 'bool' | 'enum' | 'status' | 'int'
  enum?: string[]
  // int only: inclusive bounds mirrored into client-side validation.
  min?: number
  max?: number
  required_mesh?: boolean
  required_direct?: boolean
  mesh_only?: boolean
}

export interface ProbeTypeInfo {
  type: string
  // http: the prober needs a URL, which mesh expansion cannot provide.
  // ntp: peer agents do not serve time, so a mesh template is meaningless.
  direct_only?: boolean
  params: ParamSpec[]
}

export interface ProbeTypesResponse {
  types: ProbeTypeInfo[]
}

// GET /api/v1/auth/providers — advertised before any session exists, so the
// login page knows whether to offer single sign-on.
export interface AuthProviders {
  oidc: { enabled: boolean }
}

// GET /api/v1/ui-banner — open (the sign-in screen renders the bands too).
// text is always "" while disabled, so staged text never leaks pre-auth.
export interface UIBanner {
  enabled: boolean
  text: string
}

// GET/PUT /api/v1/settings/ui-banner (admin-only)
export interface BannerSettings {
  enabled: boolean
  text: string
  updated_at: string
  updated_by: string
}

export interface BannerSettingsPut {
  enabled: boolean
  text: string
}

// GET /api/v1/users (admin-only) — dashboard accounts with their
// login-event aggregates, plus monthly sign-in totals for the activity
// chart. Deleted identities are reconstructed from sign-in audit snapshots
// and carry a null created_at. The list is filtered and paged server-side
// (q, role, status, source, limit, offset); total ignores the page window.
export interface UserAccount {
  id: string
  username: string
  role: 'admin' | 'viewer'
  auth_source: 'local' | 'oidc'
  status: 'active' | 'disabled' | 'deleted'
  login_count: number
  last_login_at: string | null // null = never logged in
  created_at: string | null // null = deleted identity
}

export interface LoginMonth {
  month: string // "YYYY-MM", a UTC calendar month — never parse with Date
  total: number
  local: number
  oidc: number
  unique_users: number
}

export interface UsersResponse {
  users: UserAccount[]
  total: number
  login_months: LoginMonth[]
}

// POST /api/v1/users and POST /api/v1/users/{id}/reset-password (same
// shape) — the generated password is returned exactly once and never
// persisted in cleartext.
export interface UserCreateResponse {
  id: string
  username: string
  role: 'admin' | 'viewer'
  password: string
}

// GET/PUT /api/v1/settings/oidc (admin-only). The client secret is
// write-only: reads carry only client_secret_set.
export interface OIDCSettings {
  enabled: boolean
  issuer: string
  client_id: string
  client_secret_set: boolean
  redirect_url: string
  scopes: string[]
  username_claim: string
  role_claim: string
  admin_values: string[]
  ca_pem: string
  updated_at: string
  updated_by: string
  warnings?: string[]
}

export interface OIDCSettingsPut {
  enabled: boolean
  issuer: string
  client_id: string
  // Empty string means "keep the stored secret".
  client_secret: string
  redirect_url: string
  scopes: string[]
  username_claim: string
  role_claim: string
  admin_values: string[]
  ca_pem: string
}

// POST /api/v1/settings/oidc/test — the IdP's advertised endpoints.
export interface OIDCDiscoveryInfo {
  issuer: string
  authorization_endpoint: string
  token_endpoint: string
  jwks_uri: string
  userinfo_endpoint?: string
}
