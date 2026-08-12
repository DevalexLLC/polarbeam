// Package httpapi is the dashboard: a session-authenticated JSON API under
// /api/v1 plus the embedded SPA. It shares the HTTPS listener run.go binds;
// agent traffic never comes through here (that is grpcapi, on the mTLS
// listener).
package httpapi

import (
	"context"
	"io/fs"
	"net/http"
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
	CreateUser(ctx context.Context, username, passwordHash, role string) (uuid.UUID, error)
	SetUserDisabled(ctx context.Context, id uuid.UUID, disabled bool) error
	DeleteUser(ctx context.Context, id uuid.UUID) error
	GetUserByID(ctx context.Context, id uuid.UUID) (*store.UserInfo, error)
	ResetLocalUserPassword(ctx context.Context, id uuid.UUID, passwordHash string) (username, role string, err error)
	UpdateOwnPassword(ctx context.Context, userID uuid.UUID, verifiedHash, passwordHash string, keepSessionID uuid.UUID) error

	ListSites(ctx context.Context) ([]store.SiteInfo, error)
	ListSitesConfig(ctx context.Context) ([]store.SiteAdminInfo, error)
	CreateSite(ctx context.Context, name string, up store.SiteUpdate) (uuid.UUID, error)
	UpdateSite(ctx context.Context, name string, up store.SiteUpdate) error
	DeleteSite(ctx context.Context, name string) (int64, error)
	SiteIDByName(ctx context.Context, name string) (uuid.UUID, error)
	ListJoinTokens(ctx context.Context) ([]store.JoinTokenInfo, error)
	CreateJoinToken(ctx context.Context, siteID uuid.UUID, createdBy string, ttl time.Duration) (string, error)
	DeleteJoinToken(ctx context.Context, id uuid.UUID) error
	ListAgents(ctx context.Context) ([]store.AgentListInfo, error)
	AgentHealthSeries(ctx context.Context, window, bucket time.Duration, excludeProbeType int16) ([]store.AgentHealthBucket, error)
	AgentProbeHealth(ctx context.Context, agentID uuid.UUID, window, bucket time.Duration) ([]store.AgentProbeHealthRow, error)
	MatrixLatest(ctx context.Context, horizon time.Duration) ([]store.MatrixRow, error)
	ExpectedPairs(ctx context.Context) ([]store.SitePair, error)
	SiteEndpoints(ctx context.Context, siteName string) (*store.SiteEndpoints, error)
	PairSeries(ctx context.Context, srcAgents, dstTargets []uuid.UUID, bucket, window time.Duration, source store.Source, latencySource string) ([]store.SeriesBucket, error)
	PairSummary(ctx context.Context, srcAgents, dstTargets []uuid.UUID, window time.Duration, source store.Source) (*store.PairSummaryRow, error)
	PairLatencySource(ctx context.Context, srcAgents, dstTargets []uuid.UUID, window time.Duration, source store.Source) (string, error)
	DirectionLatest(ctx context.Context, srcAgents, dstTargets []uuid.UUID, horizon time.Duration) ([]store.MatrixRow, error)

	GetSettings(ctx context.Context) (*store.ThresholdSettings, error)
	UpdateSettings(ctx context.Context, ts store.ThresholdSettings) (*store.ThresholdSettings, error)

	ListTargets(ctx context.Context) ([]store.TargetInfo, error)
	UpsertExternalTarget(ctx context.Context, name, address string, port int32, url string) (uuid.UUID, error)
	DeleteTarget(ctx context.Context, name string) error
	ListMeshGroups(ctx context.Context) ([]store.MeshGroupInfo, error)
	UpsertMeshGroup(ctx context.Context, name string) (uuid.UUID, error)
	DeleteMeshGroup(ctx context.Context, name string) (int64, error)
	AddMeshMember(ctx context.Context, meshName, siteName string) error
	RemoveMeshMember(ctx context.Context, meshName, siteName string) error
	ListProbeConfigs(ctx context.Context) ([]store.ProbeConfigInfo, error)
	GetProbeConfig(ctx context.Context, id uuid.UUID) (*store.ProbeConfigInfo, error)
	AddDirectProbe(ctx context.Context, siteName, targetName string, ps store.ProbeSettings, enabled bool, updatedBy string) (uuid.UUID, error)
	AddMeshProbe(ctx context.Context, meshName string, ps store.ProbeSettings, enabled bool, updatedBy string) (uuid.UUID, error)
	UpdateProbeConfig(ctx context.Context, id uuid.UUID, ps store.ProbeSettings, enabled bool, updatedBy string) error
	DeleteProbeConfig(ctx context.Context, id uuid.UUID) error

	ListOutages(ctx context.Context, window time.Duration) ([]store.OutageInfo, error)
	ListPathEvents(ctx context.Context, window time.Duration) ([]store.PathEventInfo, error)
	CurrentPaths(ctx context.Context, srcAgents, dstTargets []uuid.UUID) ([]store.CurrentPath, error)

	GetOIDCSettings(ctx context.Context) (*store.OIDCSettings, error)
	UpdateOIDCSettings(ctx context.Context, o store.OIDCSettings, keepSecret bool) (*store.OIDCSettings, int64, error)
	UpsertOIDCUser(ctx context.Context, issuer, subject, username, role string) (*store.UserInfo, error)
	CreateOIDCSession(ctx context.Context, userID uuid.UUID, tokenHash []byte, csrfToken string, expiresAt time.Time, issuer, clientID string) error
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
	// OIDC settings: GET is admin-only too — issuer, claim mapping, and
	// admin group names are IdP topology, not viewer material.
	mux.Handle("GET /api/v1/settings/oidc", adminWrite(a.handleOIDCSettingsGet))
	mux.Handle("PUT /api/v1/settings/oidc", adminWrite(a.handleOIDCSettingsPut))
	mux.Handle("POST /api/v1/settings/oidc/test", adminWrite(a.handleOIDCSettingsTest))
	mux.Handle("GET /api/v1/config/probe-types", a.withSession(a.handleProbeTypes))
	mux.Handle("GET /api/v1/config/targets", a.withSession(a.handleTargetsGet))
	mux.Handle("POST /api/v1/config/targets", adminWrite(a.handleTargetPost))
	mux.Handle("DELETE /api/v1/config/targets/{name}", adminWrite(a.handleTargetDelete))
	mux.Handle("GET /api/v1/config/meshes", a.withSession(a.handleMeshesGet))
	mux.Handle("POST /api/v1/config/meshes", adminWrite(a.handleMeshPost))
	mux.Handle("DELETE /api/v1/config/meshes/{name}", adminWrite(a.handleMeshDelete))
	mux.Handle("POST /api/v1/config/meshes/{name}/members/{site}", adminWrite(a.handleMeshMemberPost))
	mux.Handle("DELETE /api/v1/config/meshes/{name}/members/{site}", adminWrite(a.handleMeshMemberDelete))
	mux.Handle("GET /api/v1/config/probes", a.withSession(a.handleProbesGet))
	mux.Handle("POST /api/v1/config/probes", adminWrite(a.handleProbePost))
	mux.Handle("PUT /api/v1/config/probes/{id}", adminWrite(a.handleProbePut))
	mux.Handle("DELETE /api/v1/config/probes/{id}", adminWrite(a.handleProbeDelete))
	mux.Handle("GET /api/v1/config/sites", a.withSession(a.handleSitesConfigGet))
	mux.Handle("POST /api/v1/config/sites", adminWrite(a.handleSiteConfigPost))
	mux.Handle("PUT /api/v1/config/sites/{name}", adminWrite(a.handleSiteConfigPut))
	mux.Handle("DELETE /api/v1/config/sites/{name}", adminWrite(a.handleSiteConfigDelete))
	// Enrollment tokens: GET is admin-only too — token audit rows are
	// credentials metadata, not viewer material (GET /settings/oidc
	// precedent).
	mux.Handle("GET /api/v1/config/tokens", adminWrite(a.handleTokensGet))
	// Account inventory (usernames, roles, login history) is admin-only for
	// the same reason; so are the account mutations.
	mux.Handle("GET /api/v1/users", adminWrite(a.handleUsersGet))
	mux.Handle("POST /api/v1/users", adminWrite(a.handleUserPost))
	mux.Handle("PUT /api/v1/users/{id}", adminWrite(a.handleUserPut))
	mux.Handle("DELETE /api/v1/users/{id}", adminWrite(a.handleUserDelete))
	mux.Handle("POST /api/v1/users/{id}/reset-password", adminWrite(a.handleUserResetPassword))
	mux.Handle("POST /api/v1/config/tokens", adminWrite(a.handleTokenPost))
	mux.Handle("DELETE /api/v1/config/tokens/{id}", adminWrite(a.handleTokenDelete))
	mux.Handle("GET /api/v1/pairs/{a}/{b}", a.withSession(a.handlePair))
	mux.Handle("GET /api/v1/pairs/{a}/{b}/series", a.withSession(a.handleSeries))
	mux.Handle("GET /api/v1/outages", a.withSession(a.handleOutages))
	mux.Handle("GET /api/v1/path-events", a.withSession(a.handlePathEvents))
	mux.Handle("GET /api/v1/traceroute/{a}/{b}", a.withSession(a.handleTraceroute))

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

func withSessionCtx(ctx context.Context, s *store.SessionInfo) context.Context {
	return context.WithValue(ctx, sessionKey, s)
}
