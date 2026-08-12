package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
	"github.com/devalexllc/polarbeam/internal/server/auth"
	"github.com/devalexllc/polarbeam/internal/server/oidcauth"
	"github.com/devalexllc/polarbeam/internal/server/store"
)

// fakeDB implements DB in memory. Only the parts each test exercises are
// populated; unhandled paths return zero values.
type fakeDB struct {
	users       map[string]*store.UserInfo
	sessions    map[string]*store.SessionInfo // key: string(token_hash)
	outages     []store.OutageInfo
	agents      []store.AgentListInfo
	agentHealth []store.AgentHealthBucket
	// probe type the last AgentHealthSeries call was told to exclude
	lastHealthExclude int16
	probeHealth       []store.AgentProbeHealthRow
	// agent id the last AgentProbeHealth call was made for
	lastProbeHealthAgent uuid.UUID
	sites                []store.SiteInfo
	endpoints            map[string]*store.SiteEndpoints
	settings             *store.ThresholdSettings

	targets     []store.TargetInfo
	meshes      []store.MeshGroupInfo
	probes      []store.ProbeConfigInfo
	siteConfigs []store.SiteAdminInfo
	joinTokens  []store.JoinTokenInfo

	userAccounts []store.UserAccountInfo
	loginMonths  []store.LoginMonthStat
	logins       []recordedLogin // appended by RecordLogin

	oidcSettings *store.OIDCSettings
	oidcUsers    map[string]*store.UserInfo // key: oidcKey(issuer, subject)
	// beforeUpdateOIDCSettings, when set, runs at the top of
	// UpdateOIDCSettings — the seam for simulating a concurrent settings
	// write landing between the handler's read and the store transaction.
	beforeUpdateOIDCSettings func()
	// beforeUpdateOwnPassword: same seam for the self-service password
	// change — simulates an admin reset landing between the handler's
	// current-password verification and the store transaction.
	beforeUpdateOwnPassword func()
	// beforeCreateLocalSession: same seam for login — simulates a password
	// rotation landing between login's verification and the session insert.
	beforeCreateLocalSession func()

	pairSummary          *store.PairSummaryRow
	pairSeries           []store.SeriesBucket
	latencySource        string
	latencySources       map[uuid.UUID]string
	directionLatest      []store.MatrixRow
	passedLatencySources []string
	lastSource           store.Source // source passed to the last PairSeries/PairSummary call
}

func newFakeDB() *fakeDB {
	return &fakeDB{
		users:     map[string]*store.UserInfo{},
		sessions:  map[string]*store.SessionInfo{},
		oidcUsers: map[string]*store.UserInfo{},
	}
}

func (f *fakeDB) addUser(username, password, role string, disabled bool) {
	hash, err := auth.HashPassword(password)
	if err != nil {
		panic(err)
	}
	f.users[username] = &store.UserInfo{
		ID: uuid.New(), Username: username, PasswordHash: hash, Role: role, Disabled: disabled,
		AuthSource: "local",
	}
}

func (f *fakeDB) GetUserByUsername(_ context.Context, username string) (*store.UserInfo, error) {
	return f.users[username], nil
}

func (f *fakeDB) CreateSession(_ context.Context, userID uuid.UUID, tokenHash []byte, csrf string, expiresAt time.Time) error {
	var username, role, authSource string
	for _, u := range f.users {
		if u.ID == userID {
			username, role, authSource = u.Username, u.Role, u.AuthSource
		}
	}
	f.sessions[string(tokenHash)] = &store.SessionInfo{
		ID: uuid.New(), UserID: userID, Username: username, Role: role, AuthSource: authSource,
		CSRFToken: csrf, ExpiresAt: expiresAt, LastUsedAt: time.Now(),
	}
	return nil
}

func (f *fakeDB) CreateLocalSession(ctx context.Context, userID uuid.UUID, tokenHash []byte, csrf string, expiresAt time.Time, verifiedHash string) error {
	if f.beforeCreateLocalSession != nil {
		f.beforeCreateLocalSession()
	}
	u := f.userByID(userID)
	if u == nil || u.AuthSource != "local" || u.PasswordHash != verifiedHash || u.Disabled {
		return store.ErrPasswordChanged
	}
	return f.CreateSession(ctx, userID, tokenHash, csrf, expiresAt)
}

func (f *fakeDB) GetSessionByTokenHash(_ context.Context, tokenHash []byte) (*store.SessionInfo, error) {
	s := f.sessions[string(tokenHash)]
	if s == nil || time.Now().After(s.ExpiresAt) {
		return nil, nil
	}
	return s, nil
}

func (f *fakeDB) TouchSession(_ context.Context, _ uuid.UUID) error { return nil }

func (f *fakeDB) DeleteSessionByTokenHash(_ context.Context, tokenHash []byte) error {
	delete(f.sessions, string(tokenHash))
	return nil
}

func (f *fakeDB) DeleteExpiredSessions(_ context.Context) (int64, error) { return 0, nil }

type recordedLogin struct {
	UserID     uuid.UUID
	Username   string
	Role       string
	AuthSource string
}

// RecordLogin snapshots from the user row like the store's INSERT..SELECT.
func (f *fakeDB) RecordLogin(_ context.Context, userID uuid.UUID) error {
	u := f.userByID(userID)
	if u == nil {
		return fmt.Errorf("record login: user %s vanished before the event was written", userID)
	}
	f.logins = append(f.logins, recordedLogin{UserID: userID, Username: u.Username, Role: u.Role, AuthSource: u.AuthSource})
	return nil
}

func (f *fakeDB) CreateUser(_ context.Context, username, passwordHash, role string) (uuid.UUID, error) {
	if f.users[username] != nil {
		return uuid.Nil, fmt.Errorf("user %q already exists%w", username, store.ErrConflict)
	}
	u := &store.UserInfo{ID: uuid.New(), Username: username, PasswordHash: passwordHash, Role: role, AuthSource: "local"}
	f.users[username] = u
	return u.ID, nil
}

func (f *fakeDB) userByID(id uuid.UUID) *store.UserInfo {
	for _, u := range f.users {
		if u.ID == id {
			return u
		}
	}
	return nil
}

// lastActiveAdminGuard mirrors the store's refusal to disable or delete the
// only enabled admin.
func (f *fakeDB) lastActiveAdminGuard(target *store.UserInfo, verb string) error {
	if target.Role != "admin" || target.Disabled {
		return nil
	}
	for _, u := range f.users {
		if u.ID != target.ID && u.Role == "admin" && !u.Disabled {
			return nil
		}
	}
	return fmt.Errorf("cannot %s the last enabled admin%w", verb, store.ErrConflict)
}

func (f *fakeDB) SetUserDisabled(_ context.Context, id uuid.UUID, disabled bool) error {
	u := f.userByID(id)
	if u == nil {
		return fmt.Errorf("user %s does not exist%w", id, store.ErrNotFound)
	}
	if disabled {
		if err := f.lastActiveAdminGuard(u, "disable"); err != nil {
			return err
		}
	}
	u.Disabled = disabled
	return nil
}

func (f *fakeDB) DeleteUser(_ context.Context, id uuid.UUID) error {
	u := f.userByID(id)
	if u == nil {
		return fmt.Errorf("user %s does not exist%w", id, store.ErrNotFound)
	}
	if err := f.lastActiveAdminGuard(u, "delete"); err != nil {
		return err
	}
	delete(f.users, u.Username)
	return nil
}

func (f *fakeDB) GetUserByID(_ context.Context, id uuid.UUID) (*store.UserInfo, error) {
	return f.userByID(id), nil
}

func (f *fakeDB) ResetLocalUserPassword(_ context.Context, id uuid.UUID, passwordHash string) (string, string, error) {
	u := f.userByID(id)
	if u == nil {
		return "", "", fmt.Errorf("user %s does not exist%w", id, store.ErrNotFound)
	}
	if u.AuthSource != "local" {
		return "", "", fmt.Errorf("federated accounts authenticate at the identity provider%w", store.ErrConflict)
	}
	u.PasswordHash = passwordHash
	for k, s := range f.sessions {
		if s.UserID == id {
			delete(f.sessions, k)
		}
	}
	return u.Username, u.Role, nil
}

func (f *fakeDB) UpdateOwnPassword(_ context.Context, userID uuid.UUID, verifiedHash, passwordHash string, keepSessionID uuid.UUID) error {
	if f.beforeUpdateOwnPassword != nil {
		f.beforeUpdateOwnPassword()
	}
	u := f.userByID(userID)
	if u == nil || u.AuthSource != "local" {
		return fmt.Errorf("user %s does not exist%w", userID, store.ErrNotFound)
	}
	if u.PasswordHash != verifiedHash {
		return fmt.Errorf("password was changed by another request; sign in with the current password and try again%w", store.ErrConflict)
	}
	u.PasswordHash = passwordHash
	for k, s := range f.sessions {
		if s.UserID == userID && s.ID != keepSessionID {
			delete(f.sessions, k)
		}
	}
	return nil
}

// ListUserAccounts mirrors the store's filter semantics (substring match,
// exact enums, offset/limit window, total ignoring the window) so handler
// tests exercise the query-parameter plumbing.
func (f *fakeDB) ListUserAccounts(_ context.Context, flt store.UserAccountFilter) ([]store.UserAccountInfo, int64, error) {
	var matched []store.UserAccountInfo
	for _, a := range f.userAccounts {
		if flt.Query != "" && !strings.Contains(strings.ToLower(a.Username), strings.ToLower(flt.Query)) {
			continue
		}
		if flt.Role != "" && a.Role != flt.Role {
			continue
		}
		if flt.Status != "" && a.Status != flt.Status {
			continue
		}
		if flt.Source != "" && a.AuthSource != flt.Source {
			continue
		}
		matched = append(matched, a)
	}
	total := int64(len(matched))
	if flt.Offset >= len(matched) {
		return nil, total, nil
	}
	matched = matched[flt.Offset:]
	if flt.Limit > 0 && len(matched) > flt.Limit {
		matched = matched[:flt.Limit]
	}
	return matched, total, nil
}

func (f *fakeDB) MonthlyLoginStats(_ context.Context, _ int) ([]store.LoginMonthStat, error) {
	return f.loginMonths, nil
}

func (f *fakeDB) ListSites(_ context.Context) ([]store.SiteInfo, error) { return f.sites, nil }
func (f *fakeDB) ListAgents(_ context.Context) ([]store.AgentListInfo, error) {
	return f.agents, nil
}
func (f *fakeDB) AgentHealthSeries(_ context.Context, _, _ time.Duration, excludeProbeType int16) ([]store.AgentHealthBucket, error) {
	f.lastHealthExclude = excludeProbeType
	return f.agentHealth, nil
}
func (f *fakeDB) AgentProbeHealth(_ context.Context, agentID uuid.UUID, _, _ time.Duration) ([]store.AgentProbeHealthRow, error) {
	f.lastProbeHealthAgent = agentID
	return f.probeHealth, nil
}
func (f *fakeDB) MatrixLatest(_ context.Context, _ time.Duration) ([]store.MatrixRow, error) {
	return nil, nil
}
func (f *fakeDB) ExpectedPairs(_ context.Context) ([]store.SitePair, error) { return nil, nil }
func (f *fakeDB) SiteEndpoints(_ context.Context, name string) (*store.SiteEndpoints, error) {
	return f.endpoints[name], nil
}
func (f *fakeDB) PairSeries(_ context.Context, _, _ []uuid.UUID, _, _ time.Duration, source store.Source, latencySource string) ([]store.SeriesBucket, error) {
	f.lastSource = source
	f.passedLatencySources = append(f.passedLatencySources, latencySource)
	return f.pairSeries, nil
}
func (f *fakeDB) PairSummary(_ context.Context, _, _ []uuid.UUID, _ time.Duration, source store.Source) (*store.PairSummaryRow, error) {
	f.lastSource = source
	if f.pairSummary != nil {
		return f.pairSummary, nil
	}
	return &store.PairSummaryRow{}, nil
}
func (f *fakeDB) PairLatencySource(_ context.Context, srcAgents, _ []uuid.UUID, _ time.Duration, _ store.Source) (string, error) {
	if len(srcAgents) > 0 && f.latencySources != nil {
		return f.latencySources[srcAgents[0]], nil
	}
	return f.latencySource, nil
}
func (f *fakeDB) DirectionLatest(_ context.Context, _, _ []uuid.UUID, _ time.Duration) ([]store.MatrixRow, error) {
	return f.directionLatest, nil
}
func (f *fakeDB) GetSettings(_ context.Context) (*store.ThresholdSettings, error) {
	if f.settings == nil {
		// Mirrors the migration-seeded defaults.
		return &store.ThresholdSettings{
			LatencyWarnUS: 100000, LatencyCritUS: 250000,
			LossWarnPct: 1, LossCritPct: 5,
		}, nil
	}
	return f.settings, nil
}
func (f *fakeDB) UpdateSettings(_ context.Context, ts store.ThresholdSettings) (*store.ThresholdSettings, error) {
	ts.UpdatedAt = time.Now()
	f.settings = &ts
	return f.settings, nil
}
func (f *fakeDB) ListOutages(_ context.Context, _ time.Duration) ([]store.OutageInfo, error) {
	return f.outages, nil
}
func (f *fakeDB) ListPathEvents(_ context.Context, _ time.Duration) ([]store.PathEventInfo, error) {
	return nil, nil
}
func (f *fakeDB) CurrentPaths(_ context.Context, _, _ []uuid.UUID) ([]store.CurrentPath, error) {
	return nil, nil
}

func (f *fakeDB) GetOIDCSettings(_ context.Context) (*store.OIDCSettings, error) {
	if f.oidcSettings == nil {
		// Mirrors the migration-seeded defaults (disabled).
		return &store.OIDCSettings{
			Scopes:        []string{"openid", "profile", "email"},
			UsernameClaim: "preferred_username",
			RoleClaim:     "groups",
			AdminValues:   []string{},
		}, nil
	}
	return f.oidcSettings, nil
}

func (f *fakeDB) UpdateOIDCSettings(ctx context.Context, o store.OIDCSettings, keepSecret bool) (*store.OIDCSettings, int64, error) {
	if f.beforeUpdateOIDCSettings != nil {
		f.beforeUpdateOIDCSettings()
	}
	// Mirrors the store: the provider-change decision is made against the
	// CURRENT stored row, never a caller-supplied snapshot.
	cur, _ := f.GetOIDCSettings(ctx)
	providerChanged := cur.Issuer != o.Issuer || cur.ClientID != o.ClientID
	if providerChanged && keepSecret && cur.ClientSecret != "" {
		return nil, 0, store.ErrConcurrentProviderChange
	}
	var revoked int64
	if providerChanged {
		for k, s := range f.sessions {
			for _, u := range f.oidcUsers {
				if u.ID == s.UserID {
					delete(f.sessions, k)
					revoked++
					break
				}
			}
		}
	}
	if keepSecret {
		o.ClientSecret = cur.ClientSecret
	}
	o.UpdatedAt = time.Now()
	f.oidcSettings = &o
	return f.oidcSettings, revoked, nil
}

// oidcKey mirrors the store's composite identity key: subjects are unique
// only within an issuer.
func oidcKey(issuer, subject string) string { return issuer + "\x00" + subject }

// addOIDCUser pre-provisions a federated user (empty password hash).
func (f *fakeDB) addOIDCUser(issuer, subject, username, role string, disabled bool) *store.UserInfo {
	u := &store.UserInfo{
		ID: uuid.New(), Username: username, Role: role, Disabled: disabled, AuthSource: "oidc",
	}
	f.oidcUsers[oidcKey(issuer, subject)] = u
	f.users[username] = u
	return u
}

func (f *fakeDB) UpsertOIDCUser(_ context.Context, issuer, subject, username, role string) (*store.UserInfo, error) {
	if u := f.oidcUsers[oidcKey(issuer, subject)]; u != nil {
		// Username/role track the IdP; disabled survives (revocation lever).
		u.Username, u.Role = username, role
		return u, nil
	}
	u := f.addOIDCUser(issuer, subject, username, role, false)
	return u, nil
}

func (f *fakeDB) CreateOIDCSession(ctx context.Context, userID uuid.UUID, tokenHash []byte, csrf string, expiresAt time.Time, issuer, clientID string) error {
	cur, _ := f.GetOIDCSettings(ctx)
	if !cur.Enabled || cur.Issuer != issuer || cur.ClientID != clientID {
		return store.ErrProviderChanged
	}
	return f.CreateSession(ctx, userID, tokenHash, csrf, expiresAt)
}

var testDist = fstest.MapFS{
	"index.html":       {Data: []byte("<html>spa</html>")},
	"assets/app.js":    {Data: []byte("console.log('app')")},
	"assets/style.css": {Data: []byte("body{}")},
}

func newTestAPI(t *testing.T, f *fakeDB) http.Handler {
	t.Helper()
	// The default provider manager matches the default settings row: OIDC
	// off. Tests exercising the flow use newTestAPIWithProviders.
	return newHandler(f, testDist, &fakeProviders{providerErr: oidcauth.ErrDisabled})
}

func newTestAPIWithProviders(t *testing.T, f *fakeDB, p *fakeProviders) http.Handler {
	t.Helper()
	return newHandler(f, testDist, p)
}

func doLogin(t *testing.T, h http.Handler, username, password string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	req := httptest.NewRequest("POST", "/api/v1/auth/login", bytes.NewReader(body))
	req.RemoteAddr = "203.0.113.7:1234"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestLoginSuccess(t *testing.T) {
	f := newFakeDB()
	f.addUser("alice", "hunter22222", "admin", false)
	h := newTestAPI(t, f)

	w := doLogin(t, h, "alice", "hunter22222")
	if w.Code != http.StatusOK {
		t.Fatalf("login = %d, want 200: %s", w.Code, w.Body)
	}
	var res struct {
		User struct {
			Username   string `json:"username"`
			Role       string `json:"role"`
			AuthSource string `json:"auth_source"`
		}
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("bad login body: %v", err)
	}
	if res.User.Username != "alice" || res.User.Role != "admin" || res.User.AuthSource != "local" || res.CSRFToken == "" {
		t.Errorf("login body = %+v", res)
	}

	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %d, want 1", len(cookies))
	}
	c := cookies[0]
	if c.Name != sessionCookie || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.Path != "/" {
		t.Errorf("cookie flags wrong: %+v", c)
	}
	if len(f.sessions) != 1 {
		t.Errorf("sessions stored = %d, want 1", len(f.sessions))
	}
}

func TestLoginFailuresAreIdentical(t *testing.T) {
	f := newFakeDB()
	f.addUser("alice", "hunter22222", "viewer", false)
	f.addUser("mallory", "goodpassword", "viewer", true) // disabled
	h := newTestAPI(t, f)

	wrongPw := doLogin(t, h, "alice", "wrong-password")
	unknown := doLogin(t, h, "bob", "wrong-password")
	disabled := doLogin(t, h, "mallory", "goodpassword")

	for name, w := range map[string]*httptest.ResponseRecorder{
		"wrong password": wrongPw, "unknown user": unknown, "disabled user": disabled,
	} {
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: code = %d, want 401", name, w.Code)
		}
		if len(w.Result().Cookies()) != 0 {
			t.Errorf("%s: 401 must not set cookies", name)
		}
	}
	if wrongPw.Body.String() != unknown.Body.String() {
		t.Errorf("unknown-user and wrong-password bodies differ: %q vs %q",
			unknown.Body.String(), wrongPw.Body.String())
	}
}

func TestLoginRateLimit(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	var last *httptest.ResponseRecorder
	for range loginLimit + 1 {
		last = doLogin(t, h, "nobody", "irrelevant")
	}
	if last.Code != http.StatusTooManyRequests {
		t.Errorf("attempt %d = %d, want 429", loginLimit+1, last.Code)
	}
}

func TestLoginBodyTooLarge(t *testing.T) {
	f := newFakeDB()
	f.addUser("alice", "hunter22222", "admin", false)
	h := newTestAPI(t, f)

	body := `{"username":"` + strings.Repeat("a", maxRequestBody) + `","password":"hunter22222"}`
	req := httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(body))
	req.RemoteAddr = "203.0.113.7:1234"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized login = %d, want 413: %s", w.Code, w.Body)
	}
	if len(w.Result().Cookies()) != 0 {
		t.Error("413 must not set cookies")
	}
}

// TestLoginBodyLimitBypass pins the paths where a body carries a complete
// valid login object up front: a single Decode would stop at the object's
// end and never read the oversized remainder, bypassing the cap.
func TestLoginBodyLimitBypass(t *testing.T) {
	valid := `{"username":"alice","password":"hunter22222"}`
	cases := []struct {
		name string
		body io.Reader
		want int
	}{
		// strings.Reader sets Content-Length: the declared-length check
		// must reject without reading.
		{"declared length over cap", strings.NewReader(valid + strings.Repeat(" ", maxRequestBody+1)), http.StatusRequestEntityTooLarge},
		// The struct wrapper hides the reader type so no Content-Length is
		// declared (chunked-style): only the post-object EOF check can
		// surface the overflow.
		{"undeclared length over cap", struct{ io.Reader }{strings.NewReader(valid + strings.Repeat(" ", maxRequestBody+1))}, http.StatusRequestEntityTooLarge},
		{"trailing data under cap", strings.NewReader(valid + " {}"), http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDB()
			f.addUser("alice", "hunter22222", "admin", false)
			h := newTestAPI(t, f)

			req := httptest.NewRequest("POST", "/api/v1/auth/login", tc.body)
			req.RemoteAddr = "203.0.113.7:1234"
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)

			if w.Code != tc.want {
				t.Fatalf("code = %d, want %d: %s", w.Code, tc.want, w.Body)
			}
			if len(w.Result().Cookies()) != 0 {
				t.Error("rejected login must not set cookies")
			}
		})
	}
}

// loginAndCookie logs in and returns the session cookie + CSRF token.
func loginAndCookie(t *testing.T, h http.Handler, f *fakeDB) (*http.Cookie, string) {
	t.Helper()
	f.addUser("alice", "hunter22222", "viewer", false)
	w := doLogin(t, h, "alice", "hunter22222")
	if w.Code != http.StatusOK {
		t.Fatalf("login failed: %d %s", w.Code, w.Body)
	}
	var res struct {
		CSRFToken string `json:"csrf_token"`
	}
	json.Unmarshal(w.Body.Bytes(), &res)
	return w.Result().Cookies()[0], res.CSRFToken
}

func TestSessionRequired(t *testing.T) {
	h := newTestAPI(t, newFakeDB())
	for _, path := range []string{
		"/api/v1/auth/me", "/api/v1/sites", "/api/v1/agents", "/api/v1/matrix",
		"/api/v1/agents/health", "/api/v1/pairs/a/b", "/api/v1/pairs/a/b/series",
		"/api/v1/agents/00000000-0000-0000-0000-000000000000/health",
	} {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without session = %d, want 401", path, w.Code)
		}
		if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Errorf("GET %s: content-type %q, want JSON", path, ct)
		}
	}
}

func TestAgentHealth(t *testing.T) {
	f := newFakeDB()
	agentA := uuid.New()
	agentB := uuid.New()
	t0 := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	f.agentHealth = []store.AgentHealthBucket{
		{AgentID: agentA, Bucket: t0, Samples: 240, OK: 238},
		{AgentID: agentA, Bucket: t0.Add(30 * time.Minute), Samples: 240, OK: 240},
		{AgentID: agentB, Bucket: t0, Samples: 60, OK: 0},
	}
	h := newTestAPI(t, f)
	cookie, _ := loginAndCookie(t, h, f)

	req := httptest.NewRequest("GET", "/api/v1/agents/health?window=24h", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("agents/health = %d %s", w.Code, w.Body)
	}
	var res struct {
		Window  string `json:"window"`
		BucketS int    `json:"bucket_s"`
		Agents  []struct {
			ID      string `json:"id"`
			Buckets []struct {
				T       int64 `json:"t"`
				Samples int64 `json:"samples"`
				OK      int64 `json:"ok"`
			} `json:"buckets"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("bad body: %v", err)
	}
	if res.Window != "24h" || res.BucketS != 1800 {
		t.Errorf("window/bucket_s = %q/%d, want 24h/1800", res.Window, res.BucketS)
	}
	if len(res.Agents) != 2 {
		t.Fatalf("agents = %d, want 2 (rows grouped per agent)", len(res.Agents))
	}
	if res.Agents[0].ID != agentA.String() || len(res.Agents[0].Buckets) != 2 {
		t.Errorf("first agent = %s with %d buckets, want %s with 2",
			res.Agents[0].ID, len(res.Agents[0].Buckets), agentA)
	}
	if b := res.Agents[0].Buckets[0]; b.T != t0.Unix() || b.Samples != 240 || b.OK != 238 {
		t.Errorf("first bucket = %+v", b)
	}
	if res.Agents[1].ID != agentB.String() || res.Agents[1].Buckets[0].OK != 0 {
		t.Errorf("second agent = %+v", res.Agents[1])
	}
	// Traceroute run-accounting rows must be excluded from the ratio.
	if f.lastHealthExclude != int16(pb.ProbeType_PROBE_TYPE_TRACEROUTE) {
		t.Errorf("excluded probe type = %d, want traceroute (%d)",
			f.lastHealthExclude, int16(pb.ProbeType_PROBE_TYPE_TRACEROUTE))
	}

	// Empty fleet must serve an empty list, not null.
	f.agentHealth = nil
	req = httptest.NewRequest("GET", "/api/v1/agents/health", nil)
	req.AddCookie(cookie)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"agents":[]`) {
		t.Errorf("empty fleet = %d %s, want 200 with agents:[]", w.Code, w.Body)
	}

	// Only the 24h window exists.
	req = httptest.NewRequest("GET", "/api/v1/agents/health?window=7d", nil)
	req.AddCookie(cookie)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("window=7d = %d, want 400", w.Code)
	}
}

func TestAgentProbeHealth(t *testing.T) {
	f := newFakeDB()
	agent := uuid.New()
	probeA := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	probeB := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	probeC := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	t0 := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	opened := t0.Add(-2 * time.Hour)
	str := func(s string) *string { return &s }
	i64 := func(v int64) *int64 { return &v }
	tp := func(v time.Time) *time.Time { return &v }
	f.probeHealth = []store.AgentProbeHealthRow{
		// Agent-kind icmp series with an open outage: two bucket rows,
		// label columns repeated per row as the store query produces.
		{ProbeID: probeA, ProbeType: int16(pb.ProbeType_PROBE_TYPE_ICMP),
			TargetKind: str("agent"), TargetName: str("agent:deadbeef"), DstSite: str("lon"),
			LastStatus: int16(pb.ProbeStatus_PROBE_STATUS_TIMEOUT), LastTime: t0,
			OpenedAt: tp(opened), OpenError: str("i/o timeout"),
			Bucket: tp(t0), Samples: i64(30), OK: i64(0)},
		{ProbeID: probeA, ProbeType: int16(pb.ProbeType_PROBE_TYPE_ICMP),
			TargetKind: str("agent"), TargetName: str("agent:deadbeef"), DstSite: str("lon"),
			LastStatus: int16(pb.ProbeStatus_PROBE_STATUS_TIMEOUT), LastTime: t0,
			OpenedAt: tp(opened), OpenError: str("i/o timeout"),
			Bucket: tp(t0.Add(30 * time.Minute)), Samples: i64(30), OK: i64(12)},
		// External http series, healthy.
		{ProbeID: probeB, ProbeType: int16(pb.ProbeType_PROBE_TYPE_HTTP),
			TargetKind: str("external"), TargetName: str("corp-vpn"),
			LastStatus: int16(pb.ProbeStatus_PROBE_STATUS_OK), LastTime: t0,
			Bucket: tp(t0), Samples: i64(60), OK: i64(60)},
		// Silent traceroute series: the LEFT JOIN's single nil-bucket row.
		{ProbeID: probeC, ProbeType: int16(pb.ProbeType_PROBE_TYPE_TRACEROUTE),
			TargetKind: str("agent"), TargetName: str("agent:cafe"), DstSite: str("syd"),
			LastStatus: int16(pb.ProbeStatus_PROBE_STATUS_OK), LastTime: t0.Add(-48 * time.Hour)},
	}
	h := newTestAPI(t, f)
	cookie, _ := loginAndCookie(t, h, f)

	req := httptest.NewRequest("GET", "/api/v1/agents/"+agent.String()+"/health?window=24h", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("agent probe health = %d %s", w.Code, w.Body)
	}
	if f.lastProbeHealthAgent != agent {
		t.Errorf("store queried for agent %s, want %s (path id)", f.lastProbeHealthAgent, agent)
	}
	var res struct {
		Window  string `json:"window"`
		BucketS int    `json:"bucket_s"`
		Probes  []struct {
			ProbeID    string  `json:"probe_id"`
			Type       string  `json:"type"`
			TargetKind string  `json:"target_kind"`
			Target     *string `json:"target"`
			DstSite    *string `json:"dst_site"`
			LastStatus string  `json:"last_status"`
			Failing    bool    `json:"failing"`
			OpenSince  *string `json:"open_since"`
			Error      *string `json:"error"`
			Buckets    []struct {
				T       int64 `json:"t"`
				Samples int64 `json:"samples"`
				OK      int64 `json:"ok"`
			} `json:"buckets"`
		} `json:"probes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("bad body: %v", err)
	}
	if res.Window != "24h" || res.BucketS != 1800 {
		t.Errorf("window/bucket_s = %q/%d, want 24h/1800", res.Window, res.BucketS)
	}
	if len(res.Probes) != 3 {
		t.Fatalf("probes = %d, want 3 (rows grouped per probe)", len(res.Probes))
	}
	pa := res.Probes[0]
	if pa.ProbeID != probeA.String() || pa.Type != "icmp" || len(pa.Buckets) != 2 {
		t.Errorf("first probe = %s type %s with %d buckets, want %s icmp with 2",
			pa.ProbeID, pa.Type, len(pa.Buckets), probeA)
	}
	if b := pa.Buckets[0]; b.T != t0.Unix() || b.Samples != 30 || b.OK != 0 {
		t.Errorf("first bucket = %+v", b)
	}
	// Agent-kind targets label by destination site; the synthesized
	// agent:<id> targets.name must never leak into target.
	if pa.Target != nil || pa.DstSite == nil || *pa.DstSite != "lon" {
		t.Errorf("agent-kind labels = target %v dst_site %v, want null/lon", pa.Target, pa.DstSite)
	}
	if !pa.Failing || pa.OpenSince == nil || pa.Error == nil || *pa.Error != "i/o timeout" ||
		pa.LastStatus != "timeout" {
		t.Errorf("failing probe = %+v, want failing with open_since/error/timeout", pa)
	}
	pb2 := res.Probes[1]
	if pb2.Type != "http" || pb2.TargetKind != "external" ||
		pb2.Target == nil || *pb2.Target != "corp-vpn" || pb2.DstSite != nil {
		t.Errorf("external probe = %+v, want http/external/corp-vpn/null dst_site", pb2)
	}
	if pb2.Failing || pb2.OpenSince != nil || pb2.Error != nil {
		t.Errorf("healthy probe carries outage fields: %+v", pb2)
	}
	pc := res.Probes[2]
	if pc.Type != "traceroute" || len(pc.Buckets) != 0 {
		t.Errorf("silent probe = type %s with %d buckets, want traceroute with buckets:[]",
			pc.Type, len(pc.Buckets))
	}
	if !strings.Contains(w.Body.String(), `"buckets":[]`) {
		t.Errorf("silent series must serve buckets:[], not null: %s", w.Body)
	}

	// An agent with no series (or an unknown id) is an empty list, not null.
	f.probeHealth = nil
	req = httptest.NewRequest("GET", "/api/v1/agents/"+uuid.New().String()+"/health", nil)
	req.AddCookie(cookie)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"probes":[]`) {
		t.Errorf("no series = %d %s, want 200 with probes:[]", w.Code, w.Body)
	}

	// Only the 24h window exists.
	req = httptest.NewRequest("GET", "/api/v1/agents/"+agent.String()+"/health?window=7d", nil)
	req.AddCookie(cookie)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("window=7d = %d, want 400", w.Code)
	}

	// A malformed id is a 400, not a store call.
	req = httptest.NewRequest("GET", "/api/v1/agents/not-a-uuid/health", nil)
	req.AddCookie(cookie)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad id = %d, want 400", w.Code)
	}
}

func TestHealthzOpen(t *testing.T) {
	h := newTestAPI(t, newFakeDB())
	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "ok") {
		t.Errorf("healthz = %d %q", w.Code, w.Body.String())
	}
}

func TestMeAndLogoutCSRF(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, csrf := loginAndCookie(t, h, f)

	// me works with just the cookie.
	req := httptest.NewRequest("GET", "/api/v1/auth/me", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), csrf) {
		t.Fatalf("me = %d %s", w.Code, w.Body)
	}
	// The About page renders this; an empty string would render as a blank
	// field rather than failing, so pin it here.
	var me struct {
		User struct {
			AuthSource string `json:"auth_source"`
		} `json:"user"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil {
		t.Fatalf("bad me body: %v", err)
	}
	if me.Version == "" {
		t.Errorf("me carries no version: %s", w.Body)
	}
	// The user menu hides password management for federated accounts on this
	// field; an absent value would hide it for everyone.
	if me.User.AuthSource != "local" {
		t.Errorf("me auth_source = %q, want local: %s", me.User.AuthSource, w.Body)
	}

	// logout without CSRF token → 403, session survives.
	req = httptest.NewRequest("POST", "/api/v1/auth/logout", nil)
	req.AddCookie(cookie)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("logout without csrf = %d, want 403", w.Code)
	}
	if len(f.sessions) != 1 {
		t.Errorf("session deleted despite missing CSRF token")
	}

	// wrong CSRF token → 403.
	req = httptest.NewRequest("POST", "/api/v1/auth/logout", nil)
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", "wrong")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("logout with wrong csrf = %d, want 403", w.Code)
	}

	// correct CSRF token → session gone, cookie cleared.
	req = httptest.NewRequest("POST", "/api/v1/auth/logout", nil)
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", csrf)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("logout = %d %s", w.Code, w.Body)
	}
	if len(f.sessions) != 0 {
		t.Errorf("session not deleted on logout")
	}
	cleared := w.Result().Cookies()
	if len(cleared) != 1 || cleared[0].MaxAge >= 0 {
		t.Errorf("logout must clear the cookie, got %+v", cleared)
	}

	// the old cookie no longer authenticates.
	req = httptest.NewRequest("GET", "/api/v1/auth/me", nil)
	req.AddCookie(cookie)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("me after logout = %d, want 401", w.Code)
	}
}

func TestRequireRole(t *testing.T) {
	f := newFakeDB()
	h := New(f, testDist)
	cookie, _ := loginAndCookie(t, h, f) // alice is a viewer

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	a := &api{db: db{f}}
	guarded := a.withSession(func(w http.ResponseWriter, r *http.Request) {
		requireRole("admin", inner).ServeHTTP(w, r)
	})

	req := httptest.NewRequest("GET", "/anything", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	guarded.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("viewer behind requireRole(admin) = %d, want 403", w.Code)
	}
}

func TestUnknownAPIPathIsJSON404(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, _ := loginAndCookie(t, h, f)

	req := httptest.NewRequest("GET", "/api/v1/nope", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown api path = %d, want 404", w.Code)
	}
	if body := w.Body.String(); strings.Contains(body, "<html") {
		t.Errorf("API 404 served the SPA: %q", body)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("API 404 content-type = %q, want JSON", ct)
	}
}

func TestStaticSPAFallback(t *testing.T) {
	h := newTestAPI(t, newFakeDB())
	for _, path := range []string{"/", "/pair/nyc/lon", "/deep/link"} {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		body, _ := io.ReadAll(w.Result().Body)
		if w.Code != http.StatusOK || !strings.Contains(string(body), "spa") {
			t.Errorf("GET %s = %d %q, want index.html", path, w.Code, body)
		}
		if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
			t.Errorf("GET %s: missing CSP, got %q", path, csp)
		}
	}
	// real assets are served as themselves
	req := httptest.NewRequest("GET", "/assets/app.js", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "console.log") {
		t.Errorf("asset not served: %q", w.Body.String())
	}
}

func TestBadWindowAndMetric(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, _ := loginAndCookie(t, h, f)

	for _, url := range []string{
		"/api/v1/pairs/a/b?window=6d",
		"/api/v1/pairs/a/b/series?window=never",
		"/api/v1/pairs/a/b/series?metric=bogus",
	} {
		req := httptest.NewRequest("GET", url, nil)
		req.AddCookie(cookie)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", url, w.Code)
		}
	}
}

// TestPairPercentilesAndSource covers the M5 additions: aggregate-sourced
// windows carry p50/p95/p99, jitter/tcp/tls averages, and the source label;
// raw windows omit the percentile keys entirely (omitempty, not zero).
func TestPairPercentilesAndSource(t *testing.T) {
	f := newFakeDB()
	nycAgent, lonAgent := uuid.New(), uuid.New()
	f.endpoints = map[string]*store.SiteEndpoints{
		"nyc": {SiteInfo: store.SiteInfo{Name: "nyc"}, AgentIDs: []uuid.UUID{nycAgent}},
		"lon": {SiteInfo: store.SiteInfo{Name: "lon"}, AgentIDs: []uuid.UUID{lonAgent}},
	}
	f.latencySources = map[uuid.UUID]string{nycAgent: "rtt", lonAgent: "tcp_connect"}
	fv := func(v float64) *float64 { return &v }
	f.pairSummary = &store.PairSummaryRow{
		AvgUS: fv(1500), P50US: fv(1400), P95US: fv(2900), P99US: fv(4100),
		JitterAvgUS: fv(120), TCPConnectAvgUS: fv(800), TLSHandshakeAvgUS: fv(2100),
		LatencySource: "rtt", Samples: 42,
	}
	checkLoss := float32(0)
	f.directionLatest = []store.MatrixRow{{
		ProbeType: int16(pb.ProbeType_PROBE_TYPE_ICMP),
		Status:    int16(pb.ProbeStatus_PROBE_STATUS_OK),
		Time:      time.Date(2026, 7, 1, 1, 2, 3, 0, time.UTC),
		LatencyUS: new(int64(1200)), LatencySource: "rtt", LossPct: &checkLoss,
	}}
	f.pairSeries = []store.SeriesBucket{
		{Bucket: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), AvgUS: fv(1500),
			P50US: fv(1400), P95US: fv(2900), P99US: fv(4100), Samples: 12},
	}
	f.latencySource = "rtt"
	h := newTestAPI(t, f)
	cookie, _ := loginAndCookie(t, h, f)

	get := func(url string) map[string]any {
		t.Helper()
		req := httptest.NewRequest("GET", url, nil)
		req.AddCookie(cookie)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", url, w.Code, w.Body)
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		return body
	}

	pair := get("/api/v1/pairs/nyc/lon?window=90d")
	if pair["source"] != "hourly" {
		t.Errorf("pair source = %v, want hourly", pair["source"])
	}
	if f.lastSource != store.SourceHourly {
		t.Errorf("PairSummary called with source %q, want hourly", f.lastSource)
	}
	checks := pair["a_to_b"].(map[string]any)["checks"].([]any)
	if len(checks) != 1 || checks[0].(map[string]any)["type"] != "icmp" ||
		checks[0].(map[string]any)["latency_source"] != "rtt" {
		t.Errorf("pair checks = %#v, want latest ICMP detail", checks)
	}
	lat := pair["a_to_b"].(map[string]any)["latency"].(map[string]any)
	for key, want := range map[string]float64{"p50_us": 1400, "p95_us": 2900, "p99_us": 4100} {
		if lat[key] != want {
			t.Errorf("pair latency[%s] = %v, want %v", key, lat[key], want)
		}
	}
	if jit := pair["a_to_b"].(map[string]any)["jitter_avg_us"]; jit != 120.0 {
		t.Errorf("jitter_avg_us = %v, want 120", jit)
	}

	series := get("/api/v1/pairs/nyc/lon/series?window=365d")
	if series["source"] != "daily" || series["latency_source"] != "rtt" {
		t.Errorf("series source/latency_source = %v/%v, want daily/rtt", series["source"], series["latency_source"])
	}
	if f.lastSource != store.SourceDaily {
		t.Errorf("PairSeries called with source %q, want daily", f.lastSource)
	}
	if got := series["latency_source"]; got != "rtt" {
		t.Errorf("compat latency_source = %v, want a→b source rtt", got)
	}
	if got := series["a_to_b"].(map[string]any)["latency_source"]; got != "rtt" {
		t.Errorf("a→b latency_source = %v, want rtt", got)
	}
	if got := series["b_to_a"].(map[string]any)["latency_source"]; got != "tcp_connect" {
		t.Errorf("b→a latency_source = %v, want tcp_connect", got)
	}
	if len(f.passedLatencySources) < 2 ||
		f.passedLatencySources[len(f.passedLatencySources)-2] != "rtt" ||
		f.passedLatencySources[len(f.passedLatencySources)-1] != "tcp_connect" {
		t.Errorf("PairSeries latency sources = %v, want [... rtt tcp_connect]", f.passedLatencySources)
	}
	pt := series["a_to_b"].(map[string]any)["points"].([]any)[0].(map[string]any)
	if pt["p95_us"] != 2900.0 {
		t.Errorf("series point p95_us = %v, want 2900", pt["p95_us"])
	}

	// Raw window: fake returns nil percentiles, and omitempty must drop the
	// keys rather than render null/0.
	f.pairSummary = &store.PairSummaryRow{AvgUS: fv(1500), LatencySource: "rtt"}
	f.pairSeries = []store.SeriesBucket{{Bucket: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), AvgUS: fv(1500)}}
	raw := get("/api/v1/pairs/nyc/lon?window=24h")
	if raw["source"] != "raw" {
		t.Errorf("raw pair source = %v, want raw", raw["source"])
	}
	rawLat := raw["a_to_b"].(map[string]any)["latency"].(map[string]any)
	for _, key := range []string{"p50_us", "p95_us", "p99_us"} {
		if _, present := rawLat[key]; present {
			t.Errorf("raw window latency carries %s; want key absent", key)
		}
	}
	rawSeries := get("/api/v1/pairs/nyc/lon/series?window=7d")
	rawPt := rawSeries["a_to_b"].(map[string]any)["points"].([]any)[0].(map[string]any)
	if _, present := rawPt["p50_us"]; present {
		t.Error("raw series point carries p50_us; want key absent")
	}
}

func TestUnknownSiteIs404(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, _ := loginAndCookie(t, h, f)

	req := httptest.NewRequest("GET", "/api/v1/pairs/nowhere/lon?window=24h", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "nowhere") {
		t.Errorf("unknown site = %d %s, want 404 naming the site", w.Code, w.Body)
	}
}

func TestEventsEndpoints(t *testing.T) {
	f := newFakeDB()
	closed := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	dst, target, ptype := "nyc", "nyc-agent", int16(1)
	f.outages = []store.OutageInfo{
		{ID: uuid.New(), Kind: "probe_failing", AgentHostname: "syd-1", SrcSite: "syd",
			DstSite: &dst, TargetName: &target, ProbeType: &ptype,
			OpenedAt: closed.Add(-time.Hour)},
		{ID: uuid.New(), Kind: "agent_offline", AgentHostname: "lon-1", SrcSite: "lon",
			OpenedAt: closed.Add(-2 * time.Hour), ClosedAt: &closed},
	}
	h := newTestAPI(t, f)
	cookie, _ := loginAndCookie(t, h, f)

	req := httptest.NewRequest("GET", "/api/v1/outages", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("outages = %d %s", w.Code, w.Body)
	}
	body := w.Body.String()
	// probe_failing rows carry the mapped type name; agent_offline rows null it.
	if !strings.Contains(body, `"probe_type":"icmp"`) || !strings.Contains(body, `"kind":"agent_offline"`) {
		t.Errorf("outages body missing expected fields: %s", body)
	}

	// Empty results serve well-formed shapes, not nulls that break the SPA.
	for path, key := range map[string]string{
		"/api/v1/path-events":       `"events":[]`,
		"/api/v1/traceroute/ok/ok2": "", // 404s below, shape untested here
	} {
		if key == "" {
			continue
		}
		req := httptest.NewRequest("GET", path, nil)
		req.AddCookie(cookie)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), key) {
			t.Errorf("GET %s = %d %s, want 200 containing %s", path, w.Code, w.Body, key)
		}
	}

	// Bad window is a 400, unknown site in traceroute a 404 naming it.
	req = httptest.NewRequest("GET", "/api/v1/outages?window=6d", nil)
	req.AddCookie(cookie)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("outages bad window = %d, want 400", w.Code)
	}
	req = httptest.NewRequest("GET", "/api/v1/traceroute/nowhere/lon", nil)
	req.AddCookie(cookie)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "nowhere") {
		t.Errorf("traceroute unknown site = %d %s, want 404 naming it", w.Code, w.Body)
	}
}

func TestAgentsHealthFields(t *testing.T) {
	f := newFakeDB()
	seen := time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC)
	notAfter := seen.AddDate(1, 0, 0)
	dropped := seen.Add(-time.Hour)
	f.agents = []store.AgentListInfo{
		{ID: uuid.New(), Site: "lon", Hostname: "lon-1", ProbeAddress: "10.0.0.1",
			Version: "abc123", LastSeenAt: &seen, CreatedAt: seen.AddDate(0, -1, 0),
			ConfigHash: "deadbeef", CertNotAfter: &notAfter,
			Offline: true, ProbesFailing: 2, ProbesTotal: 9,
			DroppedResults: 41, LastDroppedAt: &dropped},
	}
	h := newTestAPI(t, f)
	cookie, _ := loginAndCookie(t, h, f)

	req := httptest.NewRequest("GET", "/api/v1/agents", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("agents = %d %s", w.Code, w.Body)
	}
	body := w.Body.String()
	for _, want := range []string{
		`"offline":true`, `"probes_failing":2`, `"probes_total":9`,
		`"dropped_results":41`, `"config_hash":"deadbeef"`,
		`"cert_not_after":"2027-07-31T09:00:00Z"`, `"cert_revoked_at":null`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("agents body missing %s: %s", want, body)
		}
	}
}

func TestEventsRequireSession(t *testing.T) {
	h := newTestAPI(t, newFakeDB())
	for _, path := range []string{"/api/v1/outages", "/api/v1/path-events", "/api/v1/traceroute/a/b"} {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without session = %d, want 401", path, w.Code)
		}
	}
}
