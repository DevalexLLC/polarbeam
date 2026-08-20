package httpapi

// HTTP-level tests for the scoped WRITE surface behind networkWrite.
//
// TestRouteInventoryAuthz proves admission (who reaches the handler at all).
// These prove the other half, which admission alone does not give you: the
// handler's scope proof, and that its refusal is a 404 that reads exactly
// like a name that never existed. A 403 here would be a leak — it would tell
// a tenant that the plane, mesh, or probe it guessed at is real.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/devalexllc/polarbeam/internal/server/store"
)

// tenantAPI builds a fake carrying two planes plus a tenant admin scoped to
// exactly one of them, and returns the handler and that tenant's credentials.
func tenantAPI(t *testing.T) (http.Handler, *fakeDB, *http.Cookie, string, store.NetworkRef, store.NetworkRef) {
	t.Helper()
	f := newFakeDB()
	h := newTestAPI(t, f)
	mine := store.NetworkRef{ID: uuid.New(), Name: "tenant-a"}
	theirs := store.NetworkRef{ID: uuid.New(), Name: "tenant-b"}
	for _, n := range []store.NetworkRef{mine, theirs} {
		f.networks = append(f.networks, store.NetworkAdminInfo{ID: n.ID, Name: n.Name})
	}
	cookie, csrf := testSession(f, store.RoleNetworkAdmin, []store.NetworkRef{mine})
	return h, f, cookie, csrf, mine, theirs
}

func doWrite(t *testing.T, h http.Handler, method, path, body string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "203.0.113.9:4321"
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", csrf)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func errorText(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", w.Body, err)
	}
	return body.Error
}

// TestForeignPlaneIsIndistinguishableFromUnknown is the anti-enumeration
// property: for every scoped write that names a network, the response to
// "a plane that exists but is not mine" must be byte-identical to the
// response for "a plane that does not exist at all".
func TestForeignPlaneIsIndistinguishableFromUnknown(t *testing.T) {
	cases := []struct {
		name           string
		method, path   string
		body           func(network string) string
		pathWithNet    func(network string) string
		bodyStatic     string
		wantStatusCode int
	}{
		{
			name: "create target", method: "POST", path: "/api/v1/config/targets",
			body: func(n string) string {
				return `{"name":"t1","address":"203.0.113.5","port":443,"url":"","network":"` + n + `"}`
			},
		},
		{
			name: "create mesh", method: "POST", path: "/api/v1/config/meshes",
			body: func(n string) string { return `{"name":"m1","network":"` + n + `"}` },
		},
		{
			name: "create direct probe", method: "POST", path: "/api/v1/config/probes",
			body: func(n string) string {
				return `{"site":"s1","target":"t1","type":"icmp","interval_ms":60000,"timeout_ms":5000,"network":"` + n + `"}`
			},
		},
		{
			name: "mint token", method: "POST", path: "/api/v1/config/tokens",
			body: func(n string) string { return `{"site":"s1","network":"` + n + `","ttl_ms":3600000}` },
		},
		{
			name: "network thresholds", method: "PUT",
			pathWithNet: func(n string) string { return "/api/v1/settings/network-thresholds/" + n },
			bodyStatic:  `{"latency_warn_us":1000,"latency_crit_us":2000,"loss_warn_pct":null,"loss_crit_pct":null}`,
		},
		{
			name: "path thresholds", method: "PUT",
			pathWithNet: func(n string) string {
				return "/api/v1/settings/path-thresholds/s1/s2?network=" + n
			},
			bodyStatic: `{"latency_warn_us":1000,"latency_crit_us":2000,"loss_warn_pct":null,"loss_crit_pct":null}`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, _, cookie, csrf, _, theirs := tenantAPI(t)
			probe := func(network string) *httptest.ResponseRecorder {
				path, body := c.path, c.bodyStatic
				if c.pathWithNet != nil {
					path = c.pathWithNet(network)
				}
				if c.body != nil {
					body = c.body(network)
				}
				return doWrite(t, h, c.method, path, body, cookie, csrf)
			}
			foreign := probe(theirs.Name)
			unknown := probe("no-such-plane")

			if foreign.Code != http.StatusNotFound {
				t.Errorf("foreign plane = %d (%s), want 404 — a 403 would confirm the plane exists",
					foreign.Code, foreign.Body)
			}
			if unknown.Code != http.StatusNotFound {
				t.Errorf("unknown plane = %d (%s), want 404", unknown.Code, unknown.Body)
			}
			// Same sentence once the name is normalized away.
			norm := func(w *httptest.ResponseRecorder, n string) string {
				return strings.ReplaceAll(errorText(t, w), n, "X")
			}
			if got, want := norm(foreign, theirs.Name), norm(unknown, "no-such-plane"); got != want {
				t.Errorf("foreign %q and unknown %q must be indistinguishable", got, want)
			}
		})
	}
}

// TestScopedWriterCannotOmitTheNetwork pins that a tenant admin never gets
// the "default network" fallback a global admin has. Silently defaulting
// would land a tenant's mesh, probe, or enrolling AGENT on the operator's
// plane — the last of which is a trust decision, not a convenience.
func TestScopedWriterCannotOmitTheNetwork(t *testing.T) {
	cases := []struct {
		name, method, path, body string
	}{
		{"target", "POST", "/api/v1/config/targets", `{"name":"t1","address":"203.0.113.5","port":443,"url":""}`},
		{"mesh", "POST", "/api/v1/config/meshes", `{"name":"m1"}`},
		{"probe", "POST", "/api/v1/config/probes", `{"site":"s1","target":"t1","type":"icmp","interval_ms":60000,"timeout_ms":5000}`},
		{"token", "POST", "/api/v1/config/tokens", `{"site":"s1","ttl_ms":3600000}`},
		{"all-planes path threshold", "PUT", "/api/v1/settings/path-thresholds/s1/s2", `{"latency_warn_us":1000,"latency_crit_us":2000,"loss_warn_pct":null,"loss_crit_pct":null}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, f, cookie, csrf, _, _ := tenantAPI(t)
			// The token handler resolves the site before the network, so the
			// sites must exist or we would assert on the wrong refusal.
			f.siteConfigs = append(f.siteConfigs,
				store.SiteAdminInfo{SiteInfo: store.SiteInfo{ID: uuid.New(), Name: "s1"}},
				store.SiteAdminInfo{SiteInfo: store.SiteInfo{ID: uuid.New(), Name: "s2"}})
			w := doWrite(t, h, c.method, c.path, c.body, cookie, csrf)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("omitted network = %d, want 400: %s", w.Code, w.Body)
			}
			if !strings.Contains(errorText(t, w), "network is required") {
				t.Errorf("error %q should say the network is required", errorText(t, w))
			}
		})
	}
}

// TestScopedWriterOwnPlaneSucceeds is the other side of the coin: the guard
// must actually let the tenant do its job, or the feature is decorative.
func TestScopedWriterOwnPlaneSucceeds(t *testing.T) {
	h, f, cookie, csrf, mine, _ := tenantAPI(t)
	f.siteConfigs = append(f.siteConfigs,
		store.SiteAdminInfo{SiteInfo: store.SiteInfo{ID: uuid.New(), Name: "s1"}},
		store.SiteAdminInfo{SiteInfo: store.SiteInfo{ID: uuid.New(), Name: "s2"}})

	if w := doWrite(t, h, "POST", "/api/v1/config/targets",
		`{"name":"t1","address":"203.0.113.5","port":443,"url":"","network":"`+mine.Name+`"}`,
		cookie, csrf); w.Code != http.StatusOK {
		t.Fatalf("create target on own plane = %d: %s", w.Code, w.Body)
	}
	if w := doWrite(t, h, "POST", "/api/v1/config/meshes",
		`{"name":"m1","network":"`+mine.Name+`"}`, cookie, csrf); w.Code != http.StatusOK {
		t.Fatalf("create mesh on own plane = %d: %s", w.Code, w.Body)
	}
	if w := doWrite(t, h, "POST", "/api/v1/config/tokens",
		`{"site":"s1","network":"`+mine.Name+`","ttl_ms":3600000}`, cookie, csrf); w.Code != http.StatusOK {
		t.Fatalf("mint token on own plane = %d: %s", w.Code, w.Body)
	}
	if w := doWrite(t, h, "PUT", "/api/v1/settings/network-thresholds/"+mine.Name,
		`{"latency_warn_us":1000,"latency_crit_us":2000,"loss_warn_pct":null,"loss_crit_pct":null}`,
		cookie, csrf); w.Code != http.StatusOK {
		t.Fatalf("network thresholds on own plane = %d: %s", w.Code, w.Body)
	}
	// The token it just minted must be visible to it — a tenant that can
	// create tokens it cannot list is operating blind.
	req := httptest.NewRequest("GET", "/api/v1/config/tokens", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET tokens as tenant admin = %d: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), mine.Name) {
		t.Errorf("token list %s does not contain the tenant's own token", w.Body)
	}
}

// TestScopedProbeWriteChecksTheRowsPlane covers the PUT/DELETE paths, where
// the plane comes from the stored row rather than the request.
func TestScopedProbeWriteChecksTheRowsPlane(t *testing.T) {
	h, f, cookie, csrf, mine, theirs := tenantAPI(t)
	ours := store.ProbeConfigInfo{
		ID: uuid.New(), Site: "s1", Target: "t1", Network: mine.Name,
		ProbeType: 1, Enabled: true,
	}
	foreign := store.ProbeConfigInfo{
		ID: uuid.New(), Site: "s1", Target: "t2", Network: theirs.Name,
		ProbeType: 1, Enabled: true,
	}
	f.probes = append(f.probes, ours, foreign)

	if w := doWrite(t, h, "DELETE", "/api/v1/config/probes/"+foreign.ID.String(), "", cookie, csrf); w.Code != http.StatusNotFound {
		t.Errorf("delete co-tenant probe = %d, want 404: %s", w.Code, w.Body)
	}
	missing := doWrite(t, h, "DELETE", "/api/v1/config/probes/"+uuid.NewString(), "", cookie, csrf)
	if missing.Code != http.StatusNotFound {
		t.Errorf("delete unknown probe = %d, want 404", missing.Code)
	}
	if w := doWrite(t, h, "PUT", "/api/v1/config/probes/"+foreign.ID.String(),
		`{"interval_ms":60000,"timeout_ms":5000}`, cookie, csrf); w.Code != http.StatusNotFound {
		t.Errorf("edit co-tenant probe = %d, want 404: %s", w.Code, w.Body)
	}
	if w := doWrite(t, h, "DELETE", "/api/v1/config/probes/"+ours.ID.String(), "", cookie, csrf); w.Code != http.StatusOK {
		t.Errorf("delete own probe = %d, want 200: %s", w.Code, w.Body)
	}
}

// TestScopedMeshMembershipAuthorizesViaTheMesh pins that membership writes
// authorize through the mesh's plane, not the site — sites are shared
// operator vocabulary and carry no plane of their own.
func TestScopedMeshMembershipAuthorizesViaTheMesh(t *testing.T) {
	h, f, cookie, csrf, mine, theirs := tenantAPI(t)
	f.meshes = append(f.meshes,
		store.MeshGroupInfo{ID: uuid.New(), Name: "ours", Network: mine.Name},
		store.MeshGroupInfo{ID: uuid.New(), Name: "theirs", Network: theirs.Name})
	f.siteConfigs = append(f.siteConfigs, store.SiteAdminInfo{SiteInfo: store.SiteInfo{ID: uuid.New(), Name: "shared"}})

	if w := doWrite(t, h, "POST", "/api/v1/config/meshes/theirs/members/shared", "", cookie, csrf); w.Code != http.StatusNotFound {
		t.Errorf("add member to co-tenant mesh = %d, want 404: %s", w.Code, w.Body)
	}
	if w := doWrite(t, h, "DELETE", "/api/v1/config/meshes/theirs", "", cookie, csrf); w.Code != http.StatusNotFound {
		t.Errorf("delete co-tenant mesh = %d, want 404: %s", w.Code, w.Body)
	}
	// A SHARED site joining our own mesh is fine: meshexpand pairs only
	// same-plane agents, so membership grants nothing cross-plane.
	if w := doWrite(t, h, "POST", "/api/v1/config/meshes/ours/members/shared", "", cookie, csrf); w.Code != http.StatusOK {
		t.Errorf("add shared site to own mesh = %d, want 200: %s", w.Code, w.Body)
	}
}

// TestGlobalAdminKeepsPreTenancyBehavior guards the upgrade path: none of
// the above may change what a global admin can do on an install that has
// never heard of tenants.
func TestGlobalAdminKeepsPreTenancyBehavior(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, csrf := testSession(f, store.RoleAdmin, nil)
	f.siteConfigs = append(f.siteConfigs,
		store.SiteAdminInfo{SiteInfo: store.SiteInfo{ID: uuid.New(), Name: "s1"}},
		store.SiteAdminInfo{SiteInfo: store.SiteInfo{ID: uuid.New(), Name: "s2"}})

	// No network named anywhere: target is global, mesh and token fall back
	// to 'default', and the threshold row is the all-planes one.
	if w := doWrite(t, h, "POST", "/api/v1/config/targets",
		`{"name":"t1","address":"203.0.113.5","port":443,"url":""}`, cookie, csrf); w.Code != http.StatusOK {
		t.Fatalf("global target create = %d: %s", w.Code, w.Body)
	}
	if w := doWrite(t, h, "POST", "/api/v1/config/meshes", `{"name":"m1"}`, cookie, csrf); w.Code != http.StatusOK {
		t.Fatalf("mesh create with no network = %d: %s", w.Code, w.Body)
	}
	if w := doWrite(t, h, "POST", "/api/v1/config/tokens",
		`{"site":"s1","ttl_ms":3600000}`, cookie, csrf); w.Code != http.StatusOK {
		t.Fatalf("token create with no network = %d: %s", w.Code, w.Body)
	}
	// Sites with no agents yet: the pre-tenancy path never required
	// endpoints to exist, and an operator configuring thresholds BEFORE
	// enrollment must keep working.
	w := doWrite(t, h, "PUT", "/api/v1/settings/path-thresholds/s1/s2",
		`{"latency_warn_us":1000,"latency_crit_us":2000,"loss_warn_pct":null,"loss_crit_pct":null}`,
		cookie, csrf)
	if w.Code != http.StatusOK {
		t.Fatalf("all-planes threshold on agent-less sites = %d: %s", w.Code, w.Body)
	}
	var got pathThresholdWriteResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Network != "" {
		t.Errorf("network = %q, want \"\" (the all-planes row)", got.Network)
	}
}

// TestPathThresholdDeleteHidesSites pins that DELETE applies the same scoped
// site-visibility proof PUT does. Without it the delete verb becomes a
// site-name oracle: a scoped caller could tell a nonexistent peer ("unknown
// site") from a real but invisible one, and could remove a row for a pair it
// is not allowed to see.
func TestPathThresholdDeleteHidesSites(t *testing.T) {
	h, f, cookie, csrf, mine, _ := tenantAPI(t)
	// BOTH sites exist globally — that is the whole point. "hidden" is a
	// real site the tenant has no presence at, so it must be as
	// unmentionable as a name nobody ever used. Only "visible" resolves
	// through the scoped endpoint lookup.
	f.siteConfigs = append(f.siteConfigs,
		store.SiteAdminInfo{SiteInfo: store.SiteInfo{ID: uuid.New(), Name: "visible"}},
		store.SiteAdminInfo{SiteInfo: store.SiteInfo{ID: uuid.New(), Name: "hidden"}})
	f.endpoints = map[string]*store.SiteEndpoints{
		"visible": {AgentIDs: []uuid.UUID{uuid.New()}, TargetIDs: []uuid.UUID{uuid.New()}, Networks: []string{mine.Name}},
	}
	// A row on the hidden pair exists, so an unguarded DELETE would not
	// merely leak its existence — it would remove it.
	if f.pathThresholds == nil {
		f.pathThresholds = map[string]*store.PathThresholdOverride{}
	}
	warn := int64(1000)
	f.pathThresholds[fakePairKey("hidden", "visible", mine.Name)] = &store.PathThresholdOverride{
		A: "hidden", B: "visible", Network: mine.Name, LatencyWarnUS: &warn,
	}
	del := func(a, b string) *httptest.ResponseRecorder {
		return doWrite(t, h, "DELETE",
			"/api/v1/settings/path-thresholds/"+a+"/"+b+"?network="+mine.Name, "", cookie, csrf)
	}
	hidden := del("visible", "hidden")
	absent := del("visible", "not-a-site")
	if hidden.Code != http.StatusNotFound {
		t.Errorf("hidden peer = %d, want 404: %s", hidden.Code, hidden.Body)
	}
	if absent.Code != http.StatusNotFound {
		t.Errorf("absent peer = %d, want 404: %s", absent.Code, absent.Body)
	}
	norm := func(w *httptest.ResponseRecorder, name string) string {
		return strings.ReplaceAll(errorText(t, w), name, "X")
	}
	if got, want := norm(hidden, "hidden"), norm(absent, "not-a-site"); got != want {
		t.Errorf("hidden %q and absent %q must be indistinguishable", got, want)
	}
	// The row must still be there.
	if _, ok := f.pathThresholds[fakePairKey("hidden", "visible", mine.Name)]; !ok {
		t.Error("DELETE removed a row for a pair the caller cannot see")
	}
}
