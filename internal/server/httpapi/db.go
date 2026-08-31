package httpapi

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/devalexllc/polarbeam/internal/server/store"
)

// The dashboard's store surface, split by concern so a store-side change
// lands in one small interface (and the matching per-concern fake state in
// the same concern's test file) instead of one ~90-method list. DB below
// composes them all; handlers still call through the one composed a.db.

// sessionStore is the login/logout/session lifecycle, including the two
// methods the withSession middleware uses on every authenticated request.
type sessionStore interface {
	GetUserByUsername(ctx context.Context, username string) (*store.UserInfo, error)
	CreateLocalSession(ctx context.Context, userID uuid.UUID, tokenHash []byte, csrfToken string, expiresAt time.Time, verifiedHash string) error
	GetSessionByTokenHash(ctx context.Context, tokenHash []byte) (*store.SessionInfo, error)
	TouchSession(ctx context.Context, id uuid.UUID) error
	DeleteSessionByTokenHash(ctx context.Context, tokenHash []byte) error
	DeleteExpiredSessions(ctx context.Context) (int64, error)
	RecordLogin(ctx context.Context, userID uuid.UUID) error
}

// userStore is the admin account inventory and mutations, plus the
// self-service password change.
type userStore interface {
	ListUserAccounts(ctx context.Context, f store.UserAccountFilter) ([]store.UserAccountInfo, int64, error)
	MonthlyLoginStats(ctx context.Context, months int) ([]store.LoginMonthStat, error)
	CreateUser(ctx context.Context, username, passwordHash, role string, networks []uuid.UUID) (uuid.UUID, error)
	SetUserNetworks(ctx context.Context, id uuid.UUID, networks []uuid.UUID) error
	SetUserDisabled(ctx context.Context, id uuid.UUID, disabled bool) error
	DeleteUser(ctx context.Context, id uuid.UUID) error
	GetUserByID(ctx context.Context, id uuid.UUID) (*store.UserInfo, error)
	ResetLocalUserPassword(ctx context.Context, id uuid.UUID, passwordHash string) (username, role string, err error)
	UpdateOwnPassword(ctx context.Context, userID uuid.UUID, verifiedHash, passwordHash string, keepSessionID uuid.UUID) error
}

// siteConfigStore is the site admin CRUD surface.
type siteConfigStore interface {
	ListSitesConfig(ctx context.Context, networks []uuid.UUID) ([]store.SiteAdminInfo, error)
	QuerySitesConfig(ctx context.Context, f store.SiteConfigFilter) ([]store.SiteAdminInfo, int64, error)
	GetSiteConfig(ctx context.Context, name string, networks []uuid.UUID) (*store.SiteAdminInfo, error)
	CreateSite(ctx context.Context, name string, up store.SiteUpdate) (uuid.UUID, error)
	UpdateSite(ctx context.Context, name string, up store.SiteUpdate) error
	DeleteSite(ctx context.Context, name string) (int64, error)
}

// joinTokenStore is the enrollment-token lifecycle (SiteIDByName is here
// because minting a token resolves its site first).
type joinTokenStore interface {
	SiteIDByName(ctx context.Context, name string) (uuid.UUID, error)
	ListJoinTokens(ctx context.Context, networks []uuid.UUID) ([]store.JoinTokenInfo, error)
	CreateJoinToken(ctx context.Context, siteID, networkID uuid.UUID, createdBy string, ttl time.Duration) (string, error)
	DeleteJoinToken(ctx context.Context, id uuid.UUID, scope []uuid.UUID) error
}

// networkStore is the network-plane admin CRUD plus NetworkIDByName, the
// scope-blind name resolver every concern's tenant guard funnels through
// (requireNetworkScopeName).
type networkStore interface {
	NetworkIDByName(ctx context.Context, name string) (uuid.UUID, error)
	ListNetworksConfig(ctx context.Context, networks []uuid.UUID) ([]store.NetworkAdminInfo, error)
	CreateNetwork(ctx context.Context, name, displayName string) (uuid.UUID, error)
	UpdateNetwork(ctx context.Context, name, displayName string) error
	DeleteNetwork(ctx context.Context, name string) (int64, error)
}

// agentReader is the agent inventory and fleet-health read surface.
type agentReader interface {
	ListAgents(ctx context.Context, networks []uuid.UUID) ([]store.AgentListInfo, error)
	QueryAgents(ctx context.Context, f store.AgentInventoryFilter) ([]store.AgentListInfo, store.AgentInventorySummary, error)
	AgentHealthSeries(ctx context.Context, window, bucket time.Duration, excludeProbeType int16, networks []uuid.UUID) ([]store.AgentHealthBucket, error)
	AgentProbeHealth(ctx context.Context, agentID uuid.UUID, window, bucket time.Duration, networks []uuid.UUID) ([]store.AgentProbeHealthRow, error)
	AgentBucketFailures(ctx context.Context, agentID uuid.UUID, bucketStart time.Time, bucket time.Duration, probeID *uuid.UUID, excludeProbeType int16, networks []uuid.UUID) ([]store.AgentBucketFailureGroup, error)
}

// dashboardReader is the matrix and site-pair read surface. The pair reads
// are batched (issue #126): the handlers hand the store every direction of
// a page at once and the store pipelines the statements, so a page load
// costs a fixed handful of round trips instead of O(directions).
type dashboardReader interface {
	ListSites(ctx context.Context, networks []uuid.UUID) ([]store.SiteInfo, error)
	MatrixLatest(ctx context.Context, horizon time.Duration, networks []uuid.UUID) ([]store.MatrixRow, error)
	ExpectedPairs(ctx context.Context, networks []uuid.UUID) ([]store.NetworkPair, error)
	SiteEndpointsBatch(ctx context.Context, names []string, networks []uuid.UUID) ([]*store.SiteEndpoints, error)
	PairDirectionSummaries(ctx context.Context, dirs []store.DirectionKey, window time.Duration, source store.Source, horizon time.Duration) ([]store.DirectionSummary, error)
	PairDirectionSeries(ctx context.Context, dirs []store.DirectionKey, bucket, window time.Duration, source store.Source) ([]store.DirectionSeries, error)
}

// targetDetailReader is the per-target drill-down read surface.
type targetDetailReader interface {
	TargetEndpoints(ctx context.Context, targetID uuid.UUID, networks []uuid.UUID) (*store.TargetEndpoints, error)
	TargetStageSeries(ctx context.Context, srcAgents []uuid.UUID, targetID uuid.UUID, bucket, window time.Duration, source store.Source) ([]store.StageBucket, error)
	TargetProbeHealth(ctx context.Context, targetID uuid.UUID, window, bucket time.Duration, networks []uuid.UUID) ([]store.TargetProbeHealthRow, error)
}

// thresholdStore is the global threshold settings plus the per-pair and
// per-network override layers.
type thresholdStore interface {
	GetSettings(ctx context.Context) (*store.ThresholdSettings, error)
	UpdateSettings(ctx context.Context, ts store.ThresholdSettings) (*store.ThresholdSettings, error)
	ListPathThresholds(ctx context.Context, networks []uuid.UUID) ([]store.PathThresholdOverride, error)
	UpsertPathThreshold(ctx context.Context, siteA, siteB string, networkID *uuid.UUID, o store.PathThresholdOverride) (*store.PathThresholdOverride, error)
	DeletePathThreshold(ctx context.Context, siteA, siteB string, networkID *uuid.UUID) error
	ListNetworkThresholds(ctx context.Context, networks []uuid.UUID) ([]store.NetworkThreshold, error)
	UpsertNetworkThreshold(ctx context.Context, network string, t store.NetworkThreshold, scope []uuid.UUID) (*store.NetworkThreshold, error)
	DeleteNetworkThreshold(ctx context.Context, network string, scope []uuid.UUID) error
}

// targetConfigStore is the target and mesh-group config surface (plus the
// operational target inventory read, which rides on the same rows).
type targetConfigStore interface {
	ListTargets(ctx context.Context, networks []uuid.UUID) ([]store.TargetInfo, error)
	QueryTargetsConfig(ctx context.Context, f store.TargetConfigFilter) ([]store.TargetInfo, int64, error)
	GetTargetConfig(ctx context.Context, name string, networks []uuid.UUID) (*store.TargetInfo, error)
	QueryOperationalTargets(ctx context.Context, f store.TargetInventoryFilter) ([]store.OperationalTargetInfo, store.TargetInventorySummary, store.TargetInventorySummary, error)
	UpsertExternalTarget(ctx context.Context, name, address string, port int32, url string, networkID *uuid.UUID, scope []uuid.UUID) (uuid.UUID, error)
	DeleteTarget(ctx context.Context, name string, scope []uuid.UUID) error
	ListMeshGroups(ctx context.Context, networks []uuid.UUID) ([]store.MeshGroupInfo, error)
	UpsertMeshGroup(ctx context.Context, name string, networkID *uuid.UUID) (uuid.UUID, error)
	DeleteMeshGroup(ctx context.Context, name string, scope []uuid.UUID) (int64, error)
	AddMeshMember(ctx context.Context, meshName, siteName string, scope []uuid.UUID) error
	RemoveMeshMember(ctx context.Context, meshName, siteName string, scope []uuid.UUID) error
}

// probeConfigStore is the probe-workload config surface.
type probeConfigStore interface {
	ListProbeConfigs(ctx context.Context, networks []uuid.UUID) ([]store.ProbeConfigInfo, error)
	QueryProbeConfigs(ctx context.Context, f store.ProbeConfigFilter) ([]store.ProbeConfigInfo, int64, error)
	GetProbeConfigScoped(ctx context.Context, id uuid.UUID, networks []uuid.UUID) (*store.ProbeConfigInfo, error)
	GetProbeConfig(ctx context.Context, id uuid.UUID) (*store.ProbeConfigInfo, error)
	AddDirectProbe(ctx context.Context, siteName, targetName string, networkID uuid.UUID, ps store.ProbeSettings, enabled bool, updatedBy string, scope []uuid.UUID) (uuid.UUID, error)
	AddMeshProbe(ctx context.Context, meshName string, ps store.ProbeSettings, enabled bool, updatedBy string, scope []uuid.UUID) (uuid.UUID, error)
	UpdateProbeConfig(ctx context.Context, id uuid.UUID, ps store.ProbeSettings, enabled bool, updatedBy string) error
	DeleteProbeConfig(ctx context.Context, id uuid.UUID) error
}

// eventReader is the outage, path-event, traceroute, and path-MTU read
// surface.
type eventReader interface {
	ListOutages(ctx context.Context, window time.Duration, networks []uuid.UUID, includeRoutes bool) ([]store.OutageInfo, bool, error)
	ListPathEvents(ctx context.Context, window time.Duration, networks []uuid.UUID) ([]store.PathEventInfo, error)
	QueryPathEvents(ctx context.Context, window time.Duration, f store.PathEventFilter) ([]store.PathEventInfo, int64, bool, error)
	CurrentPathsBatch(ctx context.Context, dirs []store.DirectionKey) ([][]store.CurrentPath, error)
	CurrentPathMTUsBatch(ctx context.Context, dirs []store.DirectionKey) ([][]store.CurrentPathMTU, error)
}

// bannerStore is the UI banner read/write surface.
type bannerStore interface {
	GetBannerSettings(ctx context.Context) (*store.BannerSettings, error)
	UpdateBannerSettings(ctx context.Context, b store.BannerSettings) (*store.BannerSettings, error)
}

// oidcStore is the SSO settings and federated identity surface
// (GetOIDCSettings also satisfies oidcauth.SettingsSource).
type oidcStore interface {
	GetOIDCSettings(ctx context.Context) (*store.OIDCSettings, error)
	UpdateOIDCSettings(ctx context.Context, o store.OIDCSettings, keepSecret, keepRoleRules, keepUnmatchedRole bool) (*store.OIDCSettings, int64, error)
	UpsertOIDCUser(ctx context.Context, issuer, subject, username, role string, networks []uuid.UUID, policyUpdatedAt time.Time) (*store.UserInfo, error)
	CreateOIDCSession(ctx context.Context, userID uuid.UUID, tokenHash []byte, csrfToken string, expiresAt time.Time, issuer, clientID string, policyUpdatedAt time.Time) error
}

// DB is the subset of *store.Store the dashboard needs. It is an interface
// so handler tests run offline against a fake instead of a live PostgreSQL.
type DB interface {
	sessionStore
	userStore
	siteConfigStore
	joinTokenStore
	networkStore
	agentReader
	dashboardReader
	targetDetailReader
	thresholdStore
	targetConfigStore
	probeConfigStore
	eventReader
	bannerStore
	oidcStore
}

// Catch store-side signature drift here, at the declaration, rather than at
// run.go's httpapi.New call site.
var _ DB = (*store.Store)(nil)
