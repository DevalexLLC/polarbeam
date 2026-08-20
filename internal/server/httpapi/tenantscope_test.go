package httpapi

// Tenant-scoping tests: the route-inventory authorization fence (every
// mounted route must be classified, and each class's behavior is asserted
// per role) plus the scope plumbing from session to store.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/devalexllc/polarbeam/internal/server/auth"
	"github.com/devalexllc/polarbeam/internal/server/store"
)

// testSession mints a user and a live session directly in the fake —
// bypassing the login endpoint and its rate limiter — and returns the
// session cookie and CSRF token. networks seeds the scope for the
// network-scoped roles.
func testSession(f *fakeDB, role string, networks []store.NetworkRef) (*http.Cookie, string) {
	id := uuid.New()
	username := "u-" + id.String()[:8]
	f.users[username] = &store.UserInfo{
		ID: id, Username: username, PasswordHash: "$argon2id$x", Role: role, AuthSource: "local",
	}
	if networks != nil {
		f.userScopes[id] = networks
	}
	token, tokenHash, err := auth.NewToken()
	if err != nil {
		panic(err)
	}
	csrf := "csrf-" + id.String()
	var scope []store.NetworkRef
	if store.RoleIsNetworkScoped(role) {
		scope = networks
		if scope == nil {
			scope = []store.NetworkRef{}
		}
	}
	f.sessions[string(tokenHash)] = &store.SessionInfo{
		ID: uuid.New(), UserID: id, Username: username, Role: role, AuthSource: "local",
		CSRFToken: csrf, ExpiresAt: time.Now().Add(time.Hour), LastUsedAt: time.Now(),
		Networks: scope,
	}
	return &http.Cookie{Name: sessionCookie, Value: token}, csrf
}

// routeDispositions classifies EVERY route newHandler mounts:
//
//	open    — no session required
//	session — any authenticated role may call it (scoped roles get
//	          filtered data, never a 403)
//	admin   — global admin only; every other role is 403 by requireRole's
//	          exact string compare
//
// TestRouteInventoryAuthz fails when a mounted route is missing here (or a
// listed route is no longer mounted), so a future endpoint cannot silently
// default to an unclassified surface — this table is the tenant-isolation
// regression fence. Classify new routes deliberately; a tenant-writable
// route (PR 2's networkWrite) will get its own class when it exists.
var routeDispositions = map[string]string{
	"GET /healthz": "open",
	"/api/":        "open", // JSON 404 fallback
	"/":            "open", // SPA

	"POST /api/v1/auth/login":               "open",
	"GET /api/v1/auth/providers":            "open",
	"GET /api/v1/auth/oidc/start":           "open",
	"GET /api/v1/auth/oidc/callback":        "open",
	"GET /api/v1/ui-banner":                 "open",
	"POST /api/v1/auth/logout":              "session",
	"GET /api/v1/auth/me":                   "session",
	"PUT /api/v1/auth/password":             "session",
	"GET /api/v1/sites":                     "session",
	"GET /api/v1/agents":                    "session",
	"GET /api/v1/agents/health":             "session",
	"GET /api/v1/agents/{id}/health":        "session",
	"GET /api/v1/agents/{id}/health/bucket": "session",
	"GET /api/v1/matrix":                    "session",
	"GET /api/v1/settings":                  "session",
	"GET /api/v1/config/probe-types":        "session",
	"GET /api/v1/config/targets":            "session",
	"GET /api/v1/config/meshes":             "session",
	"GET /api/v1/config/probes":             "session",
	"GET /api/v1/config/networks":           "session",
	"GET /api/v1/config/sites":              "session",
	"GET /api/v1/pairs/{a}/{b}":             "session",
	"GET /api/v1/pairs/{a}/{b}/series":      "session",
	"GET /api/v1/targets/{id}":              "session",
	"GET /api/v1/targets/{id}/series":       "session",
	"GET /api/v1/targets/{id}/stages":       "session",
	"GET /api/v1/targets/{id}/health":       "session",
	"GET /api/v1/targets/{id}/paths":        "session",
	"GET /api/v1/outages":                   "session",
	"GET /api/v1/path-events":               "session",
	"GET /api/v1/traceroute/{a}/{b}":        "session",
	"GET /api/v1/path-mtu/{a}/{b}":          "session",

	"PUT /api/v1/settings":                               "admin",
	"PUT /api/v1/settings/path-thresholds/{a}/{b}":       "admin",
	"DELETE /api/v1/settings/path-thresholds/{a}/{b}":    "admin",
	"GET /api/v1/settings/oidc":                          "admin",
	"PUT /api/v1/settings/oidc":                          "admin",
	"POST /api/v1/settings/oidc/test":                    "admin",
	"GET /api/v1/settings/ui-banner":                     "admin",
	"PUT /api/v1/settings/ui-banner":                     "admin",
	"POST /api/v1/config/targets":                        "admin",
	"DELETE /api/v1/config/targets/{name}":               "admin",
	"POST /api/v1/config/meshes":                         "admin",
	"DELETE /api/v1/config/meshes/{name}":                "admin",
	"POST /api/v1/config/meshes/{name}/members/{site}":   "admin",
	"DELETE /api/v1/config/meshes/{name}/members/{site}": "admin",
	"POST /api/v1/config/probes":                         "admin",
	"PUT /api/v1/config/probes/{id}":                     "admin",
	"DELETE /api/v1/config/probes/{id}":                  "admin",
	"POST /api/v1/config/networks":                       "admin",
	"PUT /api/v1/config/networks/{name}":                 "admin",
	"DELETE /api/v1/config/networks/{name}":              "admin",
	"POST /api/v1/config/sites":                          "admin",
	"PUT /api/v1/config/sites/{name}":                    "admin",
	"DELETE /api/v1/config/sites/{name}":                 "admin",
	"GET /api/v1/config/tokens":                          "admin",
	"POST /api/v1/config/tokens":                         "admin",
	"DELETE /api/v1/config/tokens/{id}":                  "admin",
	"GET /api/v1/users":                                  "admin",
	"POST /api/v1/users":                                 "admin",
	"PUT /api/v1/users/{id}":                             "admin",
	"DELETE /api/v1/users/{id}":                          "admin",
	"POST /api/v1/users/{id}/reset-password":             "admin",
}

// mountedRoutes parses newHandler's mux registrations out of httpapi.go —
// http.ServeMux has no introspection, and the source is the authority the
// fence needs anyway.
func mountedRoutes(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("httpapi.go")
	if err != nil {
		t.Fatalf("read httpapi.go: %v", err)
	}
	re := regexp.MustCompile(`mux\.Handle(?:Func)?\(\s*"([^"]+)"`)
	var routes []string
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		routes = append(routes, m[1])
	}
	if len(routes) == 0 {
		t.Fatal("no mux registrations found in httpapi.go — parser broken?")
	}
	return routes
}

// probeRoute fills the pattern's placeholders and performs the request.
func probeRoute(h http.Handler, pattern string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	method, path := "GET", pattern
	if i := strings.Index(pattern, " "); i > 0 {
		method, path = pattern[:i], pattern[i+1:]
	}
	for old, new := range map[string]string{
		"{a}": "pa", "{b}": "pb", "{name}": "nx", "{site}": "sx",
		"{id}": uuid.NewString(),
	} {
		path = strings.ReplaceAll(path, old, new)
	}
	req := httptest.NewRequest(method, path, strings.NewReader(""))
	req.RemoteAddr = "203.0.113.9:4321"
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestRouteInventoryAuthz(t *testing.T) {
	routes := mountedRoutes(t)

	// The fence itself: the mux and the disposition table must agree.
	seen := map[string]bool{}
	for _, r := range routes {
		if _, ok := routeDispositions[r]; !ok {
			t.Errorf("route %q is mounted but not classified in routeDispositions — decide its tenant-isolation disposition", r)
		}
		seen[r] = true
	}
	for r := range routeDispositions {
		if !seen[r] {
			t.Errorf("route %q is classified but no longer mounted — remove it from routeDispositions", r)
		}
	}

	f := newFakeDB()
	h := newTestAPI(t, f)
	tenantNet := store.NetworkRef{ID: uuid.New(), Name: "tenant-a"}
	f.networks = append(f.networks, store.NetworkAdminInfo{ID: tenantNet.ID, Name: "tenant-a"})

	type principal struct {
		name   string
		cookie *http.Cookie
		csrf   string
	}
	adminC, adminT := testSession(f, store.RoleAdmin, nil)
	viewerC, viewerT := testSession(f, store.RoleViewer, nil)
	nadmC, nadmT := testSession(f, store.RoleNetworkAdmin, []store.NetworkRef{tenantNet})
	nviewC, nviewT := testSession(f, store.RoleNetworkViewer, []store.NetworkRef{tenantNet})
	adminP := principal{"admin", adminC, adminT}
	others := []principal{
		{"viewer", viewerC, viewerT},
		{"network_admin", nadmC, nadmT},
		{"network_viewer", nviewC, nviewT},
	}

	for _, route := range routes {
		class := routeDispositions[route]
		t.Run(route, func(t *testing.T) {
			switch class {
			case "open":
				if w := probeRoute(h, route, nil, ""); w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
					t.Errorf("unauthenticated = %d, open route must not demand auth", w.Code)
				}
			case "session":
				if w := probeRoute(h, route, nil, ""); w.Code != http.StatusUnauthorized {
					t.Errorf("unauthenticated = %d, want 401", w.Code)
				}
				for _, p := range append(others, adminP) {
					// Logout kills its own session; probe it with a throwaway.
					c, cs := p.cookie, p.csrf
					if route == "POST /api/v1/auth/logout" {
						c, cs = testSession(f, store.RoleViewer, nil)
					}
					if w := probeRoute(h, route, c, cs); w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
						t.Errorf("%s = %d, any-session route must not refuse an authenticated role", p.name, w.Code)
					}
				}
			case "admin":
				if w := probeRoute(h, route, nil, ""); w.Code != http.StatusUnauthorized {
					t.Errorf("unauthenticated = %d, want 401", w.Code)
				}
				// The imperative: every non-global-admin role — the tenant
				// admin above all — is 403 on every admin surface.
				for _, p := range others {
					if w := probeRoute(h, route, p.cookie, p.csrf); w.Code != http.StatusForbidden {
						t.Errorf("%s = %d, want 403", p.name, w.Code)
					}
				}
				if w := probeRoute(h, route, adminP.cookie, adminP.csrf); w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
					t.Errorf("admin = %d, must not be refused", w.Code)
				}
			default:
				t.Fatalf("unknown disposition %q", class)
			}
		})
	}
}

// TestScopedReadsPassScope asserts the session's network scope reaches the
// store on every scoped read — non-nil (the exact allowed IDs) for a
// network-scoped role, nil (unfiltered) for a global role.
func TestScopedReadsPassScope(t *testing.T) {
	reads := []struct {
		path   string
		method string // recorded fake method name
	}{
		{"/api/v1/sites", "ListSites"},
		{"/api/v1/agents", "ListAgents"},
		{"/api/v1/agents/health", "AgentHealthSeries"},
		{"/api/v1/outages", "ListOutages"},
		{"/api/v1/path-events", "ListPathEvents"},
		{"/api/v1/settings", "ListPathThresholds"},
		{"/api/v1/config/targets", "ListTargets"},
		{"/api/v1/config/meshes", "ListMeshGroups"},
		{"/api/v1/config/probes", "ListProbeConfigs"},
		{"/api/v1/config/networks", "ListNetworksConfig"},
		{"/api/v1/config/sites", "ListSitesConfig"},
	}

	t.Run("scoped role passes its network IDs", func(t *testing.T) {
		f := newFakeDB()
		h := newTestAPI(t, f)
		net := store.NetworkRef{ID: uuid.New(), Name: "tenant-a"}
		cookie, _ := testSession(f, store.RoleNetworkViewer, []store.NetworkRef{net})
		for _, rd := range reads {
			probeRoute(h, "GET "+rd.path, cookie, "")
			got, ok := f.scopeArgs[rd.method]
			if !ok {
				t.Errorf("%s: store method %s never called", rd.path, rd.method)
				continue
			}
			if len(got) != 1 || got[0] != net.ID {
				t.Errorf("%s: scope = %v, want [%s]", rd.path, got, net.ID)
			}
		}
		// The matrix folds three scoped queries.
		probeRoute(h, "GET /api/v1/matrix", cookie, "")
		for _, m := range []string{"ListSites", "MatrixLatest", "ExpectedPairs"} {
			if got := f.scopeArgs[m]; len(got) != 1 || got[0] != net.ID {
				t.Errorf("matrix %s: scope = %v, want [%s]", m, got, net.ID)
			}
		}
	})

	t.Run("global viewer passes nil", func(t *testing.T) {
		f := newFakeDB()
		h := newTestAPI(t, f)
		cookie, _ := testSession(f, store.RoleViewer, nil)
		for _, rd := range reads {
			probeRoute(h, "GET "+rd.path, cookie, "")
			got, ok := f.scopeArgs[rd.method]
			if !ok {
				t.Errorf("%s: store method %s never called", rd.path, rd.method)
				continue
			}
			if got != nil {
				t.Errorf("%s: scope = %v, want nil (unfiltered)", rd.path, got)
			}
		}
	})
}

// TestPairNetworkParamScopeGuard: a scoped session naming an out-of-scope
// plane in ?network= gets the same 404 as an unknown name — existence must
// not leak — while its own plane works.
func TestPairNetworkParamScopeGuard(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	tenant := store.NetworkRef{ID: uuid.New(), Name: "tenant-a"}
	f.networks = append(f.networks, store.NetworkAdminInfo{ID: tenant.ID, Name: "tenant-a"})
	agentA, agentB := uuid.New(), uuid.New()
	f.endpoints = map[string]*store.SiteEndpoints{
		"pa": {SiteInfo: store.SiteInfo{Name: "pa"}, AgentIDs: []uuid.UUID{agentA},
			TargetIDs: []uuid.UUID{uuid.New()}, Networks: []string{"tenant-a"}},
		"pb": {SiteInfo: store.SiteInfo{Name: "pb"}, AgentIDs: []uuid.UUID{agentB},
			TargetIDs: []uuid.UUID{uuid.New()}, Networks: []string{"tenant-a"}},
	}
	cookie, _ := testSession(f, store.RoleNetworkViewer, []store.NetworkRef{tenant})

	// "default" exists (seeded) but is out of scope; "nope" does not exist.
	// Both must be indistinguishable 404s.
	outOfScope := probeRoute(h, "GET /api/v1/pairs/{a}/{b}?network=default", cookie, "")
	unknown := probeRoute(h, "GET /api/v1/pairs/{a}/{b}?network=nope", cookie, "")
	if outOfScope.Code != http.StatusNotFound || unknown.Code != http.StatusNotFound {
		t.Fatalf("codes = %d/%d, want 404/404", outOfScope.Code, unknown.Code)
	}
	var a, b struct {
		Error string `json:"error"`
	}
	json.Unmarshal(outOfScope.Body.Bytes(), &a)
	json.Unmarshal(unknown.Body.Bytes(), &b)
	if !strings.Contains(a.Error, `"default" does not exist`) || !strings.Contains(b.Error, `"nope" does not exist`) {
		t.Errorf("404 bodies must use the unknown-network shape: %q / %q", a.Error, b.Error)
	}

	if w := probeRoute(h, "GET /api/v1/pairs/{a}/{b}?network=tenant-a", cookie, ""); w.Code != http.StatusOK {
		t.Errorf("in-scope plane = %d, want 200: %s", w.Code, w.Body)
	}
}

// TestMeCarriesScope: /auth/me reports the scoped role's network names and
// null for global roles — the SPA's capability signal.
func TestMeCarriesScope(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	net := store.NetworkRef{ID: uuid.New(), Name: "tenant-a"}
	scopedC, _ := testSession(f, store.RoleNetworkAdmin, []store.NetworkRef{net})
	adminC, _ := testSession(f, store.RoleAdmin, nil)

	var res struct {
		User struct {
			Role     string    `json:"role"`
			Networks *[]string `json:"networks"`
		} `json:"user"`
	}
	w := probeRoute(h, "GET /api/v1/auth/me", scopedC, "")
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("bad body: %v", err)
	}
	if res.User.Role != store.RoleNetworkAdmin || res.User.Networks == nil ||
		len(*res.User.Networks) != 1 || (*res.User.Networks)[0] != "tenant-a" {
		t.Errorf("scoped /me = %+v, want network_admin over [tenant-a]", res.User)
	}

	res.User.Networks = nil
	w = probeRoute(h, "GET /api/v1/auth/me", adminC, "")
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("bad body: %v", err)
	}
	if res.User.Networks != nil {
		t.Errorf("admin /me networks = %v, want null", *res.User.Networks)
	}
}

// TestOIDCSettingsPreserveTenantPolicy: a PUT that OMITS role_rules /
// unmatched_role (the pre-tenancy SPA's body shape) must keep the stored
// values — wiping the rules or downgrading deny→viewer on an unrelated
// settings save would silently grant unmatched users visibility into every
// network. Explicit values still replace.
func TestOIDCSettingsPreserveTenantPolicy(t *testing.T) {
	newSeeded := func() (*fakeDB, http.Handler, *http.Cookie, string) {
		f := newFakeDB()
		h := newTestAPI(t, f)
		f.oidcSettings = &store.OIDCSettings{
			Scopes: []string{"openid"}, UsernameClaim: "preferred_username", RoleClaim: "groups",
			AdminValues: []string{"polarbeam-admins"},
			RoleRules: []store.OIDCRoleRule{
				{Value: "tenant-a-admins", Role: store.RoleNetworkAdmin, Networks: []string{"tenant-a"}},
			},
			UnmatchedRole: "deny",
		}
		f.networks = append(f.networks, store.NetworkAdminInfo{ID: uuid.New(), Name: "tenant-a"})
		c, csrf := testSession(f, store.RoleAdmin, nil)
		return f, h, c, csrf
	}
	put := func(h http.Handler, c *http.Cookie, csrf, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PUT", "/api/v1/settings/oidc", strings.NewReader(body))
		req.AddCookie(c)
		req.Header.Set("X-CSRF-Token", csrf)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	// The exact field set the pre-tenancy SPA form submits.
	legacyBody := `{"enabled":false,"issuer":"","client_id":"","client_secret":"","redirect_url":"",` +
		`"scopes":["openid"],"username_claim":"preferred_username","role_claim":"groups",` +
		`"admin_values":["polarbeam-admins"],"ca_pem":""}`

	t.Run("omitted fields keep stored policy", func(t *testing.T) {
		f, h, c, csrf := newSeeded()
		if w := put(h, c, csrf, legacyBody); w.Code != http.StatusOK {
			t.Fatalf("legacy PUT = %d: %s", w.Code, w.Body)
		}
		got := f.oidcSettings
		if got.UnmatchedRole != "deny" || len(got.RoleRules) != 1 || got.RoleRules[0].Value != "tenant-a-admins" {
			t.Errorf("stored after legacy PUT = %+v, want rules and deny preserved", got)
		}
	})
	t.Run("explicit values replace", func(t *testing.T) {
		f, h, c, csrf := newSeeded()
		body := strings.TrimSuffix(legacyBody, "}") + `,"role_rules":[],"unmatched_role":"viewer"}`
		if w := put(h, c, csrf, body); w.Code != http.StatusOK {
			t.Fatalf("explicit PUT = %d: %s", w.Code, w.Body)
		}
		got := f.oidcSettings
		if got.UnmatchedRole != store.RoleViewer || len(got.RoleRules) != 0 {
			t.Errorf("stored after explicit clear = %+v, want no rules and viewer", got)
		}
	})

	// The keep-stored resolution must happen against the row the store
	// LOCKS, not the handler's earlier read: a legacy PUT racing a stricter
	// concurrent policy write must keep the stricter policy, not write the
	// snapshot's older one back.
	t.Run("omitted fields keep the CONCURRENTLY written policy", func(t *testing.T) {
		f, h, c, csrf := newSeeded()
		stricter := []store.OIDCRoleRule{
			{Value: "tenant-b-admins", Role: store.RoleNetworkAdmin, Networks: []string{"tenant-b"}},
		}
		f.beforeUpdateOIDCSettings = func() {
			// Lands between the handler's read and the store transaction.
			next := *f.oidcSettings
			next.RoleRules = stricter
			next.UnmatchedRole = "deny"
			f.oidcSettings = &next
			f.beforeUpdateOIDCSettings = nil
		}
		if w := put(h, c, csrf, legacyBody); w.Code != http.StatusOK {
			t.Fatalf("legacy PUT = %d: %s", w.Code, w.Body)
		}
		got := f.oidcSettings
		if got.UnmatchedRole != "deny" || len(got.RoleRules) != 1 || got.RoleRules[0].Value != "tenant-b-admins" {
			t.Errorf("stored after racing legacy PUT = %+v, want the concurrent stricter policy kept", got)
		}
	})
}

// TestUserCreateScopedRoles: the create surface accepts the new roles with
// their network lists and refuses the invalid shapes loudly.
func TestUserCreateScopedRoles(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	f.networks = append(f.networks, store.NetworkAdminInfo{ID: uuid.New(), Name: "tenant-a"})
	adminC, adminT := testSession(f, store.RoleAdmin, nil)

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/v1/users", strings.NewReader(body))
		req.AddCookie(adminC)
		req.Header.Set("X-CSRF-Token", adminT)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	if w := post(`{"username":"t1","role":"network_admin","networks":["tenant-a"]}`); w.Code != http.StatusOK {
		t.Fatalf("scoped create = %d, want 200: %s", w.Code, w.Body)
	}
	if len(f.lastUserNetworks) != 1 {
		t.Errorf("resolved networks = %v, want 1 ID", f.lastUserNetworks)
	}
	for name, body := range map[string]string{
		"scoped role without networks": `{"username":"t2","role":"network_viewer"}`,
		"global role with networks":    `{"username":"t3","role":"viewer","networks":["tenant-a"]}`,
		"unknown network":              `{"username":"t4","role":"network_admin","networks":["ghost"]}`,
		"unknown role":                 `{"username":"t5","role":"superadmin"}`,
	} {
		if w := post(body); w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400: %s", name, w.Code, w.Body)
		}
	}
}
