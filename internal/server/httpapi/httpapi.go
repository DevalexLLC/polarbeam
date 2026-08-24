// Package httpapi is the dashboard: a session-authenticated JSON API under
// /api/v1 plus the embedded SPA. It shares the HTTPS listener run.go binds;
// agent traffic never comes through here (that is grpcapi, on the mTLS
// listener).
package httpapi

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/devalexllc/polarbeam/internal/server/oidcauth"
	"github.com/devalexllc/polarbeam/internal/server/store"
)

// maxRequestBody caps every request body (the SNI-passthrough proxy cannot,
// so this is the only enforcement point). Without it an unauthenticated
// client could stream an unbounded login body. The largest legitimate body
// is a few KB (OIDC test's ca_pem), so 1 MiB is generous headroom.
const maxRequestBody = 1 << 20

// DB is the subset of *store.Store the dashboard needs. It is an interface
// so handler tests run offline against a fake instead of a live PostgreSQL.
type DB interface {
	GetUserByUsername(ctx context.Context, username string) (*store.UserInfo, error)
	CreateLocalSession(ctx context.Context, userID uuid.UUID, tokenHash []byte, csrfToken string, expiresAt time.Time, verifiedHash string) error
	GetSessionByTokenHash(ctx context.Context, tokenHash []byte) (*store.SessionInfo, error)
	TouchSession(ctx context.Context, id uuid.UUID) error
	DeleteSessionByTokenHash(ctx context.Context, tokenHash []byte) error
	DeleteExpiredSessions(ctx context.Context) (int64, error)
	RecordLogin(ctx context.Context, userID uuid.UUID) error
	ListUserAccounts(ctx context.Context, f store.UserAccountFilter) ([]store.UserAccountInfo, int64, error)
	MonthlyLoginStats(ctx context.Context, months int) ([]store.LoginMonthStat, error)
	CreateUser(ctx context.Context, username, passwordHash, role string, networks []uuid.UUID) (uuid.UUID, error)
	SetUserNetworks(ctx context.Context, id uuid.UUID, networks []uuid.UUID) error
	SetUserDisabled(ctx context.Context, id uuid.UUID, disabled bool) error
	DeleteUser(ctx context.Context, id uuid.UUID) error
	GetUserByID(ctx context.Context, id uuid.UUID) (*store.UserInfo, error)
	ResetLocalUserPassword(ctx context.Context, id uuid.UUID, passwordHash string) (username, role string, err error)
	UpdateOwnPassword(ctx context.Context, userID uuid.UUID, verifiedHash, passwordHash string, keepSessionID uuid.UUID) error

	ListSites(ctx context.Context, networks []uuid.UUID) ([]store.SiteInfo, error)
	ListSitesConfig(ctx context.Context, networks []uuid.UUID) ([]store.SiteAdminInfo, error)
	CreateSite(ctx context.Context, name string, up store.SiteUpdate) (uuid.UUID, error)
	UpdateSite(ctx context.Context, name string, up store.SiteUpdate) error
	DeleteSite(ctx context.Context, name string) (int64, error)
	SiteIDByName(ctx context.Context, name string) (uuid.UUID, error)
	ListJoinTokens(ctx context.Context, networks []uuid.UUID) ([]store.JoinTokenInfo, error)
	CreateJoinToken(ctx context.Context, siteID, networkID uuid.UUID, createdBy string, ttl time.Duration) (string, error)
	DeleteJoinToken(ctx context.Context, id uuid.UUID, scope []uuid.UUID) error
	NetworkIDByName(ctx context.Context, name string) (uuid.UUID, error)
	ListNetworksConfig(ctx context.Context, networks []uuid.UUID) ([]store.NetworkAdminInfo, error)
	CreateNetwork(ctx context.Context, name, displayName string) (uuid.UUID, error)
	UpdateNetwork(ctx context.Context, name, displayName string) error
	DeleteNetwork(ctx context.Context, name string) (int64, error)
	ListAgents(ctx context.Context, networks []uuid.UUID) ([]store.AgentListInfo, error)
	AgentHealthSeries(ctx context.Context, window, bucket time.Duration, excludeProbeType int16, networks []uuid.UUID) ([]store.AgentHealthBucket, error)
	AgentProbeHealth(ctx context.Context, agentID uuid.UUID, window, bucket time.Duration, networks []uuid.UUID) ([]store.AgentProbeHealthRow, error)
	AgentBucketFailures(ctx context.Context, agentID uuid.UUID, bucketStart time.Time, bucket time.Duration, probeID *uuid.UUID, excludeProbeType int16, networks []uuid.UUID) ([]store.AgentBucketFailureGroup, error)
	MatrixLatest(ctx context.Context, horizon time.Duration, networks []uuid.UUID) ([]store.MatrixRow, error)
	ExpectedPairs(ctx context.Context, networks []uuid.UUID) ([]store.NetworkPair, error)
	SiteEndpoints(ctx context.Context, siteName string, networks []uuid.UUID) (*store.SiteEndpoints, error)
	PairSeries(ctx context.Context, srcAgents, dstTargets []uuid.UUID, bucket, window time.Duration, source store.Source, latencySource string) ([]store.SeriesBucket, error)
	PairSummary(ctx context.Context, srcAgents, dstTargets []uuid.UUID, window time.Duration, source store.Source) (*store.PairSummaryRow, error)
	PairLatencySource(ctx context.Context, srcAgents, dstTargets []uuid.UUID, window time.Duration, source store.Source) (string, error)
	DirectionLatest(ctx context.Context, srcAgents, dstTargets []uuid.UUID, horizon time.Duration) ([]store.MatrixRow, error)
	TargetEndpoints(ctx context.Context, targetID uuid.UUID, networks []uuid.UUID) (*store.TargetEndpoints, error)
	TargetStageSeries(ctx context.Context, srcAgents []uuid.UUID, targetID uuid.UUID, bucket, window time.Duration, source store.Source) ([]store.StageBucket, error)
	TargetProbeHealth(ctx context.Context, targetID uuid.UUID, window, bucket time.Duration, networks []uuid.UUID) ([]store.TargetProbeHealthRow, error)

	GetSettings(ctx context.Context) (*store.ThresholdSettings, error)
	UpdateSettings(ctx context.Context, ts store.ThresholdSettings) (*store.ThresholdSettings, error)
	ListPathThresholds(ctx context.Context, networks []uuid.UUID) ([]store.PathThresholdOverride, error)
	UpsertPathThreshold(ctx context.Context, siteA, siteB string, networkID *uuid.UUID, o store.PathThresholdOverride) (*store.PathThresholdOverride, error)
	DeletePathThreshold(ctx context.Context, siteA, siteB string, networkID *uuid.UUID) error
	ListNetworkThresholds(ctx context.Context, networks []uuid.UUID) ([]store.NetworkThreshold, error)
	UpsertNetworkThreshold(ctx context.Context, network string, t store.NetworkThreshold, scope []uuid.UUID) (*store.NetworkThreshold, error)
	DeleteNetworkThreshold(ctx context.Context, network string, scope []uuid.UUID) error

	ListTargets(ctx context.Context, networks []uuid.UUID) ([]store.TargetInfo, error)
	UpsertExternalTarget(ctx context.Context, name, address string, port int32, url string, networkID *uuid.UUID, scope []uuid.UUID) (uuid.UUID, error)
	DeleteTarget(ctx context.Context, name string, scope []uuid.UUID) error
	ListMeshGroups(ctx context.Context, networks []uuid.UUID) ([]store.MeshGroupInfo, error)
	UpsertMeshGroup(ctx context.Context, name string, networkID *uuid.UUID) (uuid.UUID, error)
	DeleteMeshGroup(ctx context.Context, name string, scope []uuid.UUID) (int64, error)
	AddMeshMember(ctx context.Context, meshName, siteName string, scope []uuid.UUID) error
	RemoveMeshMember(ctx context.Context, meshName, siteName string, scope []uuid.UUID) error
	ListProbeConfigs(ctx context.Context, networks []uuid.UUID) ([]store.ProbeConfigInfo, error)
	GetProbeConfig(ctx context.Context, id uuid.UUID) (*store.ProbeConfigInfo, error)
	AddDirectProbe(ctx context.Context, siteName, targetName string, networkID uuid.UUID, ps store.ProbeSettings, enabled bool, updatedBy string, scope []uuid.UUID) (uuid.UUID, error)
	AddMeshProbe(ctx context.Context, meshName string, ps store.ProbeSettings, enabled bool, updatedBy string, scope []uuid.UUID) (uuid.UUID, error)
	UpdateProbeConfig(ctx context.Context, id uuid.UUID, ps store.ProbeSettings, enabled bool, updatedBy string) error
	DeleteProbeConfig(ctx context.Context, id uuid.UUID) error

	ListOutages(ctx context.Context, window time.Duration, networks []uuid.UUID) ([]store.OutageInfo, error)
	ListPathEvents(ctx context.Context, window time.Duration, networks []uuid.UUID) ([]store.PathEventInfo, error)
	QueryPathEvents(ctx context.Context, window time.Duration, f store.PathEventFilter) ([]store.PathEventInfo, int64, bool, error)
	CurrentPaths(ctx context.Context, srcAgents, dstTargets []uuid.UUID) ([]store.CurrentPath, error)
	CurrentPathMTUs(ctx context.Context, srcAgents, dstTargets []uuid.UUID) ([]store.CurrentPathMTU, error)

	GetBannerSettings(ctx context.Context) (*store.BannerSettings, error)
	UpdateBannerSettings(ctx context.Context, b store.BannerSettings) (*store.BannerSettings, error)

	GetOIDCSettings(ctx context.Context) (*store.OIDCSettings, error)
	UpdateOIDCSettings(ctx context.Context, o store.OIDCSettings, keepSecret, keepRoleRules, keepUnmatchedRole bool) (*store.OIDCSettings, int64, error)
	UpsertOIDCUser(ctx context.Context, issuer, subject, username, role string, networks []uuid.UUID, policyUpdatedAt time.Time) (*store.UserInfo, error)
	CreateOIDCSession(ctx context.Context, userID uuid.UUID, tokenHash []byte, csrfToken string, expiresAt time.Time, issuer, clientID string, policyUpdatedAt time.Time) error
}

// OIDCProviders is the slice of oidcauth.Manager the handlers use — an
// interface for the same reason DB is one: httpapi tests fake it, so no
// test ever performs IdP discovery.
type OIDCProviders interface {
	Provider(ctx context.Context) (oidcauth.Provider, *store.OIDCSettings, error)
	Test(ctx context.Context, cfg store.OIDCSettings) (*oidcauth.DiscoveryInfo, error)
	Invalidate()
}

const (
	sessionCookie = "polarbeam_session"
	sessionTTL    = 7 * 24 * time.Hour
	// staleHorizon bounds "current" state: a series with no result inside
	// it renders as stale rather than trusting old data.
	staleHorizon = 10 * time.Minute
)

type api struct {
	db      db
	limiter *loginLimiter
	// ssoLimiter is deliberately separate from the local-login limiter:
	// SSO round-trips (or a failing IdP) behind one NAT must never burn
	// the break-glass password login's attempts.
	ssoLimiter *loginLimiter
	// pwLimiter guards the self-service password change's current-password
	// oracle. Separate for the same isolation reason, and keyed by user ID
	// rather than IP: the endpoint is authenticated, so the key is exact,
	// and exhausting it must never burn anyone's login attempts.
	pwLimiter *loginLimiter
	providers OIDCProviders
}

// db wraps DB so internal helpers hang off a private type.
type db struct{ DB }

// New returns the dashboard handler: /healthz (open), /api/v1 (sessions),
// and the SPA from static for everything else.
func New(sdb DB, static fs.FS) http.Handler {
	return newHandler(sdb, static, oidcauth.NewManager(sdb))
}

// newHandler is New with the OIDC manager injectable; tests pass a fake.
func newHandler(sdb DB, static fs.FS, providers OIDCProviders) http.Handler {
	a := &api{
		db:         db{sdb},
		limiter:    newLoginLimiter(loginLimit, loginWindow),
		ssoLimiter: newLoginLimiter(loginLimit, loginWindow),
		pwLimiter:  newLoginLimiter(loginLimit, loginWindow),
		providers:  providers,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("POST /api/v1/auth/login", a.handleLogin)
	mux.Handle("POST /api/v1/auth/logout", a.withSession(a.handleLogout))
	mux.Handle("GET /api/v1/auth/me", a.withSession(a.handleMe))
	mux.Handle("PUT /api/v1/auth/password", a.withSession(a.handlePasswordChange))
	// SSO: providers is open (the login page must know whether to offer the
	// button); start/callback are open, rate-limited top-level navigations.
	mux.HandleFunc("GET /api/v1/auth/providers", a.handleAuthProviders)
	mux.HandleFunc("GET /api/v1/auth/oidc/start", a.handleOIDCStart)
	mux.HandleFunc("GET /api/v1/auth/oidc/callback", a.handleOIDCCallback)
	mux.Handle("GET /api/v1/sites", a.withSession(a.handleSites))
	mux.Handle("GET /api/v1/agents", a.withSession(a.handleAgents))
	mux.Handle("GET /api/v1/agents/health", a.withSession(a.handleAgentHealth))
	// The literal /agents/health above wins over this wildcard for that path.
	mux.Handle("GET /api/v1/agents/{id}/health", a.withSession(a.handleAgentProbeHealth))
	// The drill-down behind one health-strip slot: why was this bucket not ok.
	mux.Handle("GET /api/v1/agents/{id}/health/bucket", a.withSession(a.handleAgentHealthBucket))
	mux.Handle("GET /api/v1/matrix", a.withSession(a.handleMatrix))
	mux.Handle("GET /api/v1/settings", a.withSession(a.handleSettingsGet))
	// withSession outermost: it populates the session context requireRole
	// reads and enforces CSRF on the mutating method.
	mux.Handle("PUT /api/v1/settings",
		a.withSession(requireRole("admin", http.HandlerFunc(a.handleSettingsPut)).ServeHTTP))
	// Probe-workload config: reads any-session, writes admin-only (the
	// same withSession-outermost ordering as PUT /settings).
	adminWrite := func(h http.HandlerFunc) http.Handler {
		return a.withSession(requireRole("admin", h).ServeHTTP)
	}
	// networkWrite admits the global admin AND the tenant admin. It is a
	// gate, not a grant: every handler mounted here MUST prove the touched
	// resource's network is in the caller's scope before it mutates
	// anything — requireNetworkScope / requireNetworkScopeName / the
	// store's own scope arguments — and must answer 404, never 403, when it
	// is not, so an out-of-scope plane is indistinguishable from one that
	// does not exist.
	//
	// Deliberately a SEPARATE wrapper rather than a widening of
	// requireRole: adminWrite's exact string compare is what keeps Users,
	// Authentication, Banner, Networks, Sites, and the global thresholds
	// closed to tenants without any code of their own, today and for
	// whatever gets mounted behind it next. Moving a route here is an
	// explicit act, and tenantscope_test.go's route inventory makes it a
	// visible one.
	networkWrite := func(h http.HandlerFunc) http.Handler {
		return a.withSession(requireRoles(h, store.RoleAdmin, store.RoleNetworkAdmin).ServeHTTP)
	}
	// OIDC settings: GET is admin-only too — issuer, claim mapping, and
	// admin group names are IdP topology, not viewer material.
	// Per-site-pair threshold overrides ride on GET /settings; only the
	// writes get their own routes. Either site order addresses the same row.
	mux.Handle("PUT /api/v1/settings/path-thresholds/{a}/{b}", networkWrite(a.handlePathThresholdPut))
	mux.Handle("DELETE /api/v1/settings/path-thresholds/{a}/{b}", networkWrite(a.handlePathThresholdDelete))
	mux.Handle("PUT /api/v1/settings/network-thresholds/{network}", networkWrite(a.handleNetworkThresholdPut))
	mux.Handle("DELETE /api/v1/settings/network-thresholds/{network}", networkWrite(a.handleNetworkThresholdDelete))
	mux.Handle("GET /api/v1/settings/oidc", adminWrite(a.handleOIDCSettingsGet))
	mux.Handle("PUT /api/v1/settings/oidc", adminWrite(a.handleOIDCSettingsPut))
	mux.Handle("POST /api/v1/settings/oidc/test", adminWrite(a.handleOIDCSettingsTest))
	// UI banner: the open read reveals only what every visitor sees rendered
	// anyway (and no text at all while disabled); edits are admin-only, and
	// so is the admin read — it carries updated_by usernames.
	mux.HandleFunc("GET /api/v1/ui-banner", a.handleUIBannerGet)
	mux.Handle("GET /api/v1/settings/ui-banner", adminWrite(a.handleBannerSettingsGet))
	mux.Handle("PUT /api/v1/settings/ui-banner", adminWrite(a.handleBannerSettingsPut))
	mux.Handle("GET /api/v1/config/probe-types", a.withSession(a.handleProbeTypes))
	mux.Handle("GET /api/v1/config/targets", a.withSession(a.handleTargetsGet))
	mux.Handle("POST /api/v1/config/targets", networkWrite(a.handleTargetPost))
	mux.Handle("DELETE /api/v1/config/targets/{name}", networkWrite(a.handleTargetDelete))
	mux.Handle("GET /api/v1/config/meshes", a.withSession(a.handleMeshesGet))
	mux.Handle("POST /api/v1/config/meshes", networkWrite(a.handleMeshPost))
	mux.Handle("DELETE /api/v1/config/meshes/{name}", networkWrite(a.handleMeshDelete))
	mux.Handle("POST /api/v1/config/meshes/{name}/members/{site}", networkWrite(a.handleMeshMemberPost))
	mux.Handle("DELETE /api/v1/config/meshes/{name}/members/{site}", networkWrite(a.handleMeshMemberDelete))
	mux.Handle("GET /api/v1/config/probes", a.withSession(a.handleProbesGet))
	mux.Handle("POST /api/v1/config/probes", networkWrite(a.handleProbePost))
	mux.Handle("PUT /api/v1/config/probes/{id}", networkWrite(a.handleProbePut))
	mux.Handle("DELETE /api/v1/config/probes/{id}", networkWrite(a.handleProbeDelete))
	mux.Handle("GET /api/v1/config/networks", a.withSession(a.handleNetworksGet))
	mux.Handle("POST /api/v1/config/networks", adminWrite(a.handleNetworkPost))
	mux.Handle("PUT /api/v1/config/networks/{name}", adminWrite(a.handleNetworkPut))
	mux.Handle("DELETE /api/v1/config/networks/{name}", adminWrite(a.handleNetworkDelete))
	mux.Handle("GET /api/v1/config/sites", a.withSession(a.handleSitesConfigGet))
	mux.Handle("POST /api/v1/config/sites", adminWrite(a.handleSiteConfigPost))
	mux.Handle("PUT /api/v1/config/sites/{name}", adminWrite(a.handleSiteConfigPut))
	mux.Handle("DELETE /api/v1/config/sites/{name}", adminWrite(a.handleSiteConfigDelete))
	// Enrollment tokens: GET is admin-only too — token audit rows are
	// credentials metadata, not viewer material (GET /settings/oidc
	// precedent).
	// Tokens are network-scoped both ways: a tenant admin that could mint a
	// token but not list it would be operating blind.
	mux.Handle("GET /api/v1/config/tokens", networkWrite(a.handleTokensGet))
	// Account inventory (usernames, roles, login history) is admin-only for
	// the same reason; so are the account mutations.
	mux.Handle("GET /api/v1/users", adminWrite(a.handleUsersGet))
	mux.Handle("POST /api/v1/users", adminWrite(a.handleUserPost))
	mux.Handle("PUT /api/v1/users/{id}", adminWrite(a.handleUserPut))
	mux.Handle("DELETE /api/v1/users/{id}", adminWrite(a.handleUserDelete))
	mux.Handle("POST /api/v1/users/{id}/reset-password", adminWrite(a.handleUserResetPassword))
	mux.Handle("POST /api/v1/config/tokens", networkWrite(a.handleTokenPost))
	mux.Handle("DELETE /api/v1/config/tokens/{id}", networkWrite(a.handleTokenDelete))
	mux.Handle("GET /api/v1/pairs/{a}/{b}", a.withSession(a.handlePair))
	mux.Handle("GET /api/v1/pairs/{a}/{b}/series", a.withSession(a.handleSeries))
	// Target detail: reads any-session (the /config/targets precedent). The
	// strips' slot drill-down reuses /agents/{id}/health/bucket.
	mux.Handle("GET /api/v1/targets/{id}", a.withSession(a.handleTargetSummary))
	mux.Handle("GET /api/v1/targets/{id}/series", a.withSession(a.handleTargetSeries))
	mux.Handle("GET /api/v1/targets/{id}/stages", a.withSession(a.handleTargetStages))
	mux.Handle("GET /api/v1/targets/{id}/health", a.withSession(a.handleTargetHealth))
	mux.Handle("GET /api/v1/targets/{id}/paths", a.withSession(a.handleTargetPaths))
	mux.Handle("GET /api/v1/outages", a.withSession(a.handleOutages))
	mux.Handle("GET /api/v1/path-events", a.withSession(a.handlePathEvents))
	mux.Handle("GET /api/v1/traceroute/{a}/{b}", a.withSession(a.handleTraceroute))
	mux.Handle("GET /api/v1/path-mtu/{a}/{b}", a.withSession(a.handlePathMTU))

	// Unmatched API paths are JSON 404s; the SPA fallback must never
	// shadow the API namespace.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not found")
	})

	mux.Handle("/", staticHandler(static))

	return withAPIHeaders(withBodyLimit(mux))
}

// withBodyLimit caps request bodies at maxRequestBody. A declared
// Content-Length over the cap is rejected before any read; otherwise
// overflow surfaces as *http.MaxBytesError from the handler's read (which
// therefore must read to EOF — a decode that stops early would leave
// oversized trailing data unmeasured), and the connection is closed after
// the response.
func withBodyLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > maxRequestBody {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		next.ServeHTTP(w, r)
	})
}

// withAPIHeaders sets response headers common to the API namespace.
func withAPIHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Path) >= 5 && r.URL.Path[:5] == "/api/" {
			h := w.Header()
			h.Set("Cache-Control", "no-store")
			h.Set("X-Content-Type-Options", "nosniff")
		}
		next.ServeHTTP(w, r)
	})
}

type ctxKey int

const sessionKey ctxKey = 0

// sessionFrom returns the authenticated session placed by withSession.
func sessionFrom(ctx context.Context) *store.SessionInfo {
	s, _ := ctx.Value(sessionKey).(*store.SessionInfo)
	return s
}

// scopeIDs is the session's network scope as store-query input: nil for
// global roles (unfiltered) and the allowed network IDs for the scoped
// roles, where an empty non-nil slice matches nothing (fails closed).
// Every scoped read handler resolves its visibility through this one
// helper and passes the result straight to the store.
func scopeIDs(ctx context.Context) []uuid.UUID {
	s := sessionFrom(ctx)
	if s == nil {
		return []uuid.UUID{}
	}
	scope, ok := s.NetworkScope()
	if !ok {
		return nil
	}
	ids := make([]uuid.UUID, len(scope))
	for i, n := range scope {
		ids[i] = n.ID
	}
	return ids
}

// scopeNames is the session's allowed network names (nil = unscoped),
// mirroring scopeIDs for the places that reason about names — the
// ?network= guard and the /auth/me response.
func scopeNames(ctx context.Context) []string {
	s := sessionFrom(ctx)
	if s == nil {
		return []string{}
	}
	scope, ok := s.NetworkScope()
	if !ok {
		return nil
	}
	names := make([]string, len(scope))
	for i, n := range scope {
		names[i] = n.Name
	}
	return names
}

// requireNetworkScope reports whether the caller may WRITE on networkID,
// answering the request itself when not. Global roles (nil scope) always
// pass; a scoped caller passes only for its own planes.
//
// The refusal is 404, never 403, and its wording matches the one a
// nonexistent network produces: a tenant that could tell "forbidden" from
// "no such thing" could enumerate the other planes on the control plane one
// guess at a time. Every handler behind networkWrite calls this — or
// requireNetworkScopeName — before it mutates anything.
func (a *api) requireNetworkScope(w http.ResponseWriter, r *http.Request, networkID uuid.UUID, name string) bool {
	scope := scopeIDs(r.Context())
	if scope == nil || slices.Contains(scope, networkID) {
		return true
	}
	writeError(w, http.StatusNotFound, fmt.Sprintf("network %q does not exist", name))
	return false
}

// requireNetworkScopeName resolves a network NAME to its id under the
// caller's scope. The scope check runs BEFORE existence resolution — the
// same ordering pairEndpoints uses for ?network= — so an out-of-scope plane
// and a typo produce byte-identical 404s. Resolving first would let a tenant
// distinguish another tenant's plane from a name that was never taken.
//
// This is the only correct way for a scoped write to turn a network name
// into an id: store.NetworkIDByName is deliberately scope-blind, so calling
// it directly from a networkWrite handler resolves foreign planes happily.
func (a *api) requireNetworkScopeName(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	notFound := func() { writeError(w, http.StatusNotFound, fmt.Sprintf("network %q does not exist", name)) }
	if names := scopeNames(r.Context()); names != nil && !slices.Contains(names, name) {
		notFound()
		return uuid.Nil, false
	}
	id, err := a.db.NetworkIDByName(r.Context(), name)
	if err != nil {
		writeStoreError(w, "resolve network", err)
		return uuid.Nil, false
	}
	return id, true
}

// callerIsScoped reports whether the session's role limits it to a set of
// networks. Handlers use it where a scoped caller owes MORE input than a
// global one — a tenant admin must name the plane it is writing on, while
// an admin omitting it means "global"/"all planes".
func callerIsScoped(ctx context.Context) bool { return scopeIDs(ctx) != nil }

func withSessionCtx(ctx context.Context, s *store.SessionInfo) context.Context {
	return context.WithValue(ctx, sessionKey, s)
}
