package httpapi

// End-to-end tenant-scope tests over the REAL store and SQL, gated on
// POLARBEAM_TEST_DB_URL (see internal/server/dbtest).
//
// The fake-backed suites prove the handlers PASS a scope argument
// (recordScope) and refuse with 404s; nothing before this file proved the
// SQL behind the store actually USES that scope. These tests run the same
// HTTP requests through newHandler over a live *store.Store — dropping a
// `network_id = ANY($n)` predicate from a store query now fails a test
// here, where the fake suite would keep passing.
//
// In package httpapi (newHandler is unexported); reuses doWrite/errorText
// from tenantwrite_test.go and testDist/fakeProviders from the fake suite.

import (
	"context"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/devalexllc/polarbeam/internal/server/auth"
	"github.com/devalexllc/polarbeam/internal/server/dbtest"
	"github.com/devalexllc/polarbeam/internal/server/oidcauth"
	"github.com/devalexllc/polarbeam/internal/server/store"
)

// e2eEnv is two fully populated planes behind a real handler. Plane A is
// the scoped tenant's own; everything on plane B exists only to be
// invisible.
type e2eEnv struct {
	ctx context.Context
	s   *store.Store
	h   http.Handler

	netA, netB uuid.UUID

	agentA, agentB  uuid.UUID
	bucketStart     time.Time
	extA, extB      uuid.UUID // external target IDs
	probeA, probeB  uuid.UUID // direct probe IDs
	probeADel       uuid.UUID // sacrificial: own-plane delete control
	tokenA, tokenB  uuid.UUID
	extADelName     string
	meshADel        string
	tenantA         *http.Cookie
	tenantACSRF     string
	globalAdmin     *http.Cookie
	globalAdminCSRF string
}

const e2eHash = "$argon2id$test-hash"

// e2eSession mints a user and session directly in the store —
// CreateLocalSession verifies no password, so this skips argon2 and the
// login limiter (the real-store analogue of the fake's testSession).
func e2eSession(t *testing.T, ctx context.Context, s *store.Store, role string, networks []uuid.UUID) (*http.Cookie, string) {
	t.Helper()
	username := "u-" + uuid.NewString()[:8]
	id, err := s.CreateUser(ctx, username, e2eHash, role, networks)
	if err != nil {
		t.Fatalf("CreateUser %s: %v", username, err)
	}
	token, tokenHash, err := auth.NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	csrf := "csrf-" + uuid.NewString()
	if err := s.CreateLocalSession(ctx, id, tokenHash, csrf, time.Now().Add(time.Hour), e2eHash); err != nil {
		t.Fatalf("CreateLocalSession: %v", err)
	}
	return &http.Cookie{Name: sessionCookie, Value: token}, csrf
}

func e2eEnroll(t *testing.T, ctx context.Context, s *store.Store, site, hostname string, network uuid.UUID) uuid.UUID {
	t.Helper()
	siteID, err := s.EnsureSite(ctx, site)
	if err != nil {
		t.Fatalf("EnsureSite %q: %v", site, err)
	}
	token, err := s.CreateJoinToken(ctx, siteID, network, "e2e", time.Hour)
	if err != nil {
		t.Fatalf("CreateJoinToken: %v", err)
	}
	id, _, err := s.EnrollAgent(ctx, token, hostname, hostname+":9443", "v0", []byte(hostname),
		func(uuid.UUID) (store.IssuedCert, error) {
			return store.IssuedCert{
				Serial:    big.NewInt(time.Now().UnixNano()),
				NotBefore: time.Now().Add(-time.Hour),
				NotAfter:  time.Now().Add(time.Hour),
			}, nil
		})
	if err != nil {
		t.Fatalf("EnrollAgent %q: %v", hostname, err)
	}
	return id
}

func e2eSetup(t *testing.T) *e2eEnv {
	t.Helper()
	url := dbtest.Migrated(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	s, err := store.Connect(ctx, url, 10*time.Second, 0)
	if err != nil {
		t.Fatalf("store.Connect: %v", err)
	}
	t.Cleanup(s.Close)

	env := &e2eEnv{ctx: ctx, s: s,
		h: newHandler(s, testDist, &fakeProviders{providerErr: oidcauth.ErrDisabled})}

	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}

	env.netA, err = s.CreateNetwork(ctx, "tenant-a", "Tenant A")
	must("CreateNetwork tenant-a", err)
	env.netB, err = s.CreateNetwork(ctx, "tenant-b", "Tenant B")
	must("CreateNetwork tenant-b", err)

	// Agents, meshes, and mesh probes per plane. Distinct site names per
	// plane so read responses are attributable.
	env.agentA = e2eEnroll(t, ctx, s, "site-a1", "agent-a1", env.netA)
	e2eEnroll(t, ctx, s, "site-a2", "agent-a2", env.netA)
	env.agentB = e2eEnroll(t, ctx, s, "site-b1", "agent-b1", env.netB)
	e2eEnroll(t, ctx, s, "site-b2", "agent-b2", env.netB)
	for _, m := range []struct {
		name  string
		net   uuid.UUID
		sites []string
	}{
		{"mesh-a", env.netA, []string{"site-a1", "site-a2"}},
		{"mesh-b", env.netB, []string{"site-b1", "site-b2"}},
	} {
		net := m.net
		_, err := s.UpsertMeshGroup(ctx, m.name, &net)
		must("UpsertMeshGroup "+m.name, err)
		for _, site := range m.sites {
			must("AddMeshMember "+site, s.AddMeshMember(ctx, m.name, site, nil))
		}
		_, err = s.AddMeshProbe(ctx, m.name, store.ProbeSettings{
			ProbeType: 1, Interval: time.Minute, Timeout: 5 * time.Second, Params: map[string]string{},
		}, true, "e2e", nil)
		must("AddMeshProbe "+m.name, err)
	}
	// A sacrificial own-plane mesh for the delete control.
	env.meshADel = "mesh-a-del"
	netA := env.netA
	_, err = s.UpsertMeshGroup(ctx, env.meshADel, &netA)
	must("UpsertMeshGroup mesh-a-del", err)

	// External targets and direct probes per plane, plus own-plane
	// sacrificial rows so delete controls do not eat the read fixtures.
	env.extA, err = s.UpsertExternalTarget(ctx, "ext-a", "192.0.2.10", 443, "", &netA, nil)
	must("UpsertExternalTarget ext-a", err)
	netB := env.netB
	env.extB, err = s.UpsertExternalTarget(ctx, "ext-b", "192.0.2.20", 443, "", &netB, nil)
	must("UpsertExternalTarget ext-b", err)
	env.extADelName = "ext-a-del"
	_, err = s.UpsertExternalTarget(ctx, env.extADelName, "192.0.2.11", 443, "", &netA, nil)
	must("UpsertExternalTarget ext-a-del", err)

	settings := store.ProbeSettings{ProbeType: 1, Interval: time.Minute, Timeout: 5 * time.Second, Params: map[string]string{}}
	env.probeA, err = s.AddDirectProbe(ctx, "site-a1", "ext-a", env.netA, settings, true, "e2e", nil)
	must("AddDirectProbe A", err)
	env.probeB, err = s.AddDirectProbe(ctx, "site-b1", "ext-b", env.netB, settings, true, "e2e", nil)
	must("AddDirectProbe B", err)
	env.probeADel, err = s.AddDirectProbe(ctx, "site-a2", "ext-a", env.netA, settings, true, "e2e", nil)
	must("AddDirectProbe A del", err)

	// One live join token per plane (the enrollment tokens above are
	// consumed and thus invisible to ListJoinTokens).
	siteA1, err := s.EnsureSite(ctx, "site-a1")
	must("EnsureSite site-a1", err)
	siteB1, err := s.EnsureSite(ctx, "site-b1")
	must("EnsureSite site-b1", err)
	_, err = s.CreateJoinToken(ctx, siteA1, env.netA, "e2e", time.Hour)
	must("CreateJoinToken A", err)
	_, err = s.CreateJoinToken(ctx, siteB1, env.netB, "e2e", time.Hour)
	must("CreateJoinToken B", err)
	tokens, err := s.ListJoinTokens(ctx, nil)
	must("ListJoinTokens", err)
	for _, tok := range tokens {
		if tok.UsedAt != nil {
			continue // consumed enrollment tokens are undeletable audit records
		}
		switch tok.Network {
		case "tenant-a":
			env.tokenA = tok.ID
		case "tenant-b":
			env.tokenB = tok.ID
		}
	}
	if env.tokenA == uuid.Nil || env.tokenB == uuid.Nil {
		t.Fatalf("seeded join tokens not found in listing: %+v", tokens)
	}

	// A failing plane-B probe series with one failed result in a known
	// 30-minute bucket, inserted directly (production rows come from
	// ingest): the per-agent health routes have real rows to hide, and the
	// global-admin control something to show.
	env.bucketStart = time.Now().Add(-time.Hour).Truncate(30 * time.Minute)
	_, err = s.Pool().Exec(ctx, `
		INSERT INTO series_state (agent_id, probe_id, target_id, probe_type, consec_fails, last_status, last_time)
		VALUES ($1, $2, $3, 1, 3, 2, now())`,
		env.agentB, env.probeB, env.extB)
	must("seed series_state B", err)
	_, err = s.Pool().Exec(ctx, `
		INSERT INTO probe_results (time, agent_id, target_id, probe_id, probe_type, status, sent, received, error)
		VALUES ($1, $2, $3, $4, 1, 2, 3, 0, 'e2e seeded failure')`,
		env.bucketStart.Add(5*time.Minute), env.agentB, env.extB, env.probeB)
	must("seed probe_results B", err)

	// Outage and path events on plane B, inserted directly (production rows
	// come from the sweepers): the scoped event reads have something real
	// to hide, and the global-admin control something to show.
	_, err = s.Pool().Exec(ctx, `
		INSERT INTO outage_events (kind, agent_id, probe_id, target_id, probe_type, opened_at)
		VALUES ('probe_failing', $1, $2, $3, 1, now() - interval '1 hour')`,
		env.agentB, env.probeB, env.extB)
	must("seed outage_events B", err)
	_, err = s.Pool().Exec(ctx, `
		INSERT INTO path_events (time, agent_id, probe_id, target_id, old_path_hash, new_path_hash, old_hops, new_hops)
		VALUES (now() - interval '1 hour', $1, $2, $3, '\x01', '\x02', '[]', '[]')`,
		env.agentB, env.probeB, env.extB)
	must("seed path_events B", err)

	// Thresholds on plane B, so scoped writes have a real row to miss.
	lossCrit := 2.0
	latencyCrit := int64(500_000)
	_, err = s.UpsertNetworkThreshold(ctx, "tenant-b",
		store.NetworkThreshold{LossCritPct: &lossCrit, UpdatedBy: "e2e"}, nil)
	must("UpsertNetworkThreshold B", err)
	_, err = s.UpsertPathThreshold(ctx, "site-b1", "site-b2", &netB,
		store.PathThresholdOverride{LatencyCritUS: &latencyCrit, UpdatedBy: "e2e"})
	must("UpsertPathThreshold B", err)

	env.tenantA, env.tenantACSRF = e2eSession(t, ctx, s, store.RoleNetworkAdmin, []uuid.UUID{env.netA})
	env.globalAdmin, env.globalAdminCSRF = e2eSession(t, ctx, s, store.RoleAdmin, nil)
	return env
}

func e2eGet(t *testing.T, h http.Handler, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.RemoteAddr = "203.0.113.9:4321"
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// requireParity404 asserts the two responses are 404 with byte-identical
// bodies once the probed name is normalized away — the anti-enumeration
// property: a foreign-plane resource must read exactly like one that never
// existed.
func requireParity404(t *testing.T, foreign, unknown *httptest.ResponseRecorder, foreignName, unknownName string) {
	t.Helper()
	if foreign.Code != http.StatusNotFound {
		t.Errorf("foreign resource = %d (%s), want 404 — anything else confirms it exists", foreign.Code, foreign.Body)
	}
	if unknown.Code != http.StatusNotFound {
		t.Errorf("unknown resource = %d (%s), want 404", unknown.Code, unknown.Body)
	}
	if foreign.Code != http.StatusNotFound || unknown.Code != http.StatusNotFound {
		return
	}
	norm := func(w *httptest.ResponseRecorder, name string) string {
		return strings.ReplaceAll(errorText(t, w), name, "X")
	}
	if got, want := norm(foreign, foreignName), norm(unknown, unknownName); got != want {
		t.Errorf("foreign %q and unknown %q must be indistinguishable", got, want)
	}
}

func TestTenantScopeEndToEnd(t *testing.T) {
	t.Parallel()
	env := e2eSetup(t)
	h := env.h
	write := func(method, path, body string) *httptest.ResponseRecorder {
		return doWrite(t, h, method, path, body, env.tenantA, env.tenantACSRF)
	}

	t.Run("target write scope", func(t *testing.T) {
		requireParity404(t,
			write("POST", "/api/v1/config/targets", `{"name":"tx","address":"192.0.2.30","port":443,"url":"","network":"tenant-b"}`),
			write("POST", "/api/v1/config/targets", `{"name":"tx","address":"192.0.2.30","port":443,"url":"","network":"no-such-plane"}`),
			"tenant-b", "no-such-plane")
		requireParity404(t,
			write("DELETE", "/api/v1/config/targets/ext-b", ""),
			write("DELETE", "/api/v1/config/targets/no-such-target", ""),
			"ext-b", "no-such-target")
		// Own-plane controls prove the guard is not a blanket 404.
		if w := write("POST", "/api/v1/config/targets", `{"name":"tx","address":"192.0.2.30","port":443,"url":"","network":"tenant-a"}`); w.Code != http.StatusOK {
			t.Errorf("create target on own plane = %d: %s", w.Code, w.Body)
		}
		if w := write("DELETE", "/api/v1/config/targets/"+env.extADelName, ""); w.Code != http.StatusOK {
			t.Errorf("delete own target = %d: %s", w.Code, w.Body)
		}
	})

	t.Run("probe write scope", func(t *testing.T) {
		requireParity404(t,
			write("POST", "/api/v1/config/probes", `{"site":"site-b1","target":"ext-b","type":"icmp","interval_ms":60000,"timeout_ms":5000,"network":"tenant-b"}`),
			write("POST", "/api/v1/config/probes", `{"site":"site-b1","target":"ext-b","type":"icmp","interval_ms":60000,"timeout_ms":5000,"network":"no-such-plane"}`),
			"tenant-b", "no-such-plane")
		// PUT/DELETE by id: UpdateProbeConfig/DeleteProbeConfig take no
		// scope argument — only the handler's GetProbeConfigScoped
		// pre-check stands between a tenant and a co-tenant's probe row.
		unknownID := uuid.NewString()
		requireParity404(t,
			write("PUT", "/api/v1/config/probes/"+env.probeB.String(), `{"interval_ms":30000,"timeout_ms":5000}`),
			write("PUT", "/api/v1/config/probes/"+unknownID, `{"interval_ms":30000,"timeout_ms":5000}`),
			env.probeB.String(), unknownID)
		requireParity404(t,
			write("DELETE", "/api/v1/config/probes/"+env.probeB.String(), ""),
			write("DELETE", "/api/v1/config/probes/"+unknownID, ""),
			env.probeB.String(), unknownID)
		if w := write("PUT", "/api/v1/config/probes/"+env.probeA.String(), `{"interval_ms":30000,"timeout_ms":5000}`); w.Code != http.StatusOK {
			t.Errorf("edit own probe = %d: %s", w.Code, w.Body)
		}
		if w := write("DELETE", "/api/v1/config/probes/"+env.probeADel.String(), ""); w.Code != http.StatusOK {
			t.Errorf("delete own probe = %d: %s", w.Code, w.Body)
		}
	})

	t.Run("mesh write scope", func(t *testing.T) {
		requireParity404(t,
			write("POST", "/api/v1/config/meshes/mesh-b/members/site-a1", ""),
			write("POST", "/api/v1/config/meshes/no-such-mesh/members/site-a1", ""),
			"mesh-b", "no-such-mesh")
		requireParity404(t,
			write("DELETE", "/api/v1/config/meshes/mesh-b", ""),
			write("DELETE", "/api/v1/config/meshes/no-such-mesh", ""),
			"mesh-b", "no-such-mesh")
		if w := write("POST", "/api/v1/config/meshes/"+env.meshADel+"/members/site-a1", ""); w.Code != http.StatusOK {
			t.Errorf("add member to own mesh = %d: %s", w.Code, w.Body)
		}
		if w := write("DELETE", "/api/v1/config/meshes/"+env.meshADel, ""); w.Code != http.StatusOK {
			t.Errorf("delete own mesh = %d: %s", w.Code, w.Body)
		}
	})

	t.Run("token write scope", func(t *testing.T) {
		requireParity404(t,
			write("POST", "/api/v1/config/tokens", `{"site":"site-a1","network":"tenant-b","ttl_ms":3600000}`),
			write("POST", "/api/v1/config/tokens", `{"site":"site-a1","network":"no-such-plane","ttl_ms":3600000}`),
			"tenant-b", "no-such-plane")
		unknownID := uuid.NewString()
		requireParity404(t,
			write("DELETE", "/api/v1/config/tokens/"+env.tokenB.String(), ""),
			write("DELETE", "/api/v1/config/tokens/"+unknownID, ""),
			env.tokenB.String(), unknownID)
		if w := write("POST", "/api/v1/config/tokens", `{"site":"site-a1","network":"tenant-a","ttl_ms":3600000}`); w.Code != http.StatusOK {
			t.Errorf("mint token on own plane = %d: %s", w.Code, w.Body)
		}
		if w := write("DELETE", "/api/v1/config/tokens/"+env.tokenA.String(), ""); w.Code != http.StatusOK {
			t.Errorf("delete own token = %d: %s", w.Code, w.Body)
		}
	})

	t.Run("threshold write scope", func(t *testing.T) {
		body := `{"latency_warn_us":1000,"latency_crit_us":2000,"loss_warn_pct":null,"loss_crit_pct":null}`
		requireParity404(t,
			write("PUT", "/api/v1/settings/network-thresholds/tenant-b", body),
			write("PUT", "/api/v1/settings/network-thresholds/no-such-plane", body),
			"tenant-b", "no-such-plane")
		requireParity404(t,
			write("DELETE", "/api/v1/settings/network-thresholds/tenant-b", ""),
			write("DELETE", "/api/v1/settings/network-thresholds/no-such-plane", ""),
			"tenant-b", "no-such-plane")
		requireParity404(t,
			write("PUT", "/api/v1/settings/path-thresholds/site-b1/site-b2?network=tenant-b", body),
			write("PUT", "/api/v1/settings/path-thresholds/site-b1/site-b2?network=no-such-plane", body),
			"tenant-b", "no-such-plane")
		requireParity404(t,
			write("DELETE", "/api/v1/settings/path-thresholds/site-b1/site-b2?network=tenant-b", ""),
			write("DELETE", "/api/v1/settings/path-thresholds/site-b1/site-b2?network=no-such-plane", ""),
			"tenant-b", "no-such-plane")
		if w := write("PUT", "/api/v1/settings/network-thresholds/tenant-a", body); w.Code != http.StatusOK {
			t.Errorf("network threshold on own plane = %d: %s", w.Code, w.Body)
		}
		if w := write("PUT", "/api/v1/settings/path-thresholds/site-a1/site-a2?network=tenant-a", body); w.Code != http.StatusOK {
			t.Errorf("path threshold on own plane = %d: %s", w.Code, w.Body)
		}
	})

	t.Run("agent read scope", func(t *testing.T) {
		w := e2eGet(t, h, "/api/v1/agents", env.tenantA)
		if w.Code != http.StatusOK {
			t.Fatalf("GET /agents = %d: %s", w.Code, w.Body)
		}
		if body := w.Body.String(); !strings.Contains(body, "agent-a1") ||
			strings.Contains(body, "agent-b1") || strings.Contains(body, env.agentB.String()) {
			t.Errorf("scoped agent list must show plane A only: %s", body)
		}
		// The global admin control proves the marker WOULD appear.
		if body := e2eGet(t, h, "/api/v1/agents", env.globalAdmin).Body.String(); !strings.Contains(body, "agent-b1") {
			t.Fatalf("global admin agent list is missing plane B — absence assertions above are vacuous: %s", body)
		}
		// The per-agent health routes answer an unknown agent with scoped
		// emptiness rather than a 404; the property that matters is that a
		// FOREIGN agent reads byte-identically to a NONEXISTENT one, and
		// that neither leaks plane-B identifiers.
		epoch := env.bucketStart.Unix() // 30-minute-aligned, contains the seeded failure
		for _, route := range []struct{ foreign, unknown string }{
			{"/api/v1/agents/" + env.agentB.String() + "/health",
				"/api/v1/agents/" + uuid.NewString() + "/health"},
			{fmt.Sprintf("/api/v1/agents/%s/health/bucket?t=%d", env.agentB, epoch),
				fmt.Sprintf("/api/v1/agents/%s/health/bucket?t=%d", uuid.NewString(), epoch)},
		} {
			foreign := e2eGet(t, h, route.foreign, env.tenantA)
			unknown := e2eGet(t, h, route.unknown, env.tenantA)
			if foreign.Code != http.StatusOK {
				t.Errorf("GET %s = %d, want the scoped-empty 200: %s", route.foreign, foreign.Code, foreign.Body)
			}
			if foreign.Code != unknown.Code || foreign.Body.String() != unknown.Body.String() {
				t.Errorf("foreign agent (%d %s) and unknown agent (%d %s) must be indistinguishable at %s",
					foreign.Code, foreign.Body, unknown.Code, unknown.Body, route.foreign)
			}
			if body := foreign.Body.String(); strings.Contains(body, "ext-b") || strings.Contains(body, env.probeB.String()) {
				t.Errorf("GET %s leaks plane-B identifiers: %s", route.foreign, body)
			}
			// Anti-vacuity: the seeded failing series/result MUST surface
			// for the global admin, or the emptiness above proves nothing.
			admin := e2eGet(t, h, route.foreign, env.globalAdmin)
			if admin.Code != http.StatusOK {
				t.Fatalf("global admin GET %s = %d: %s", route.foreign, admin.Code, admin.Body)
			}
			if body := admin.Body.String(); !strings.Contains(body, env.probeB.String()) {
				t.Errorf("global admin GET %s shows no plane-B probe — the scoped emptiness assertion is vacuous: %s",
					route.foreign, body)
			}
		}
		if w := e2eGet(t, h, "/api/v1/agents/"+env.agentA.String()+"/health", env.tenantA); w.Code != http.StatusOK {
			t.Errorf("own agent health = %d: %s", w.Code, w.Body)
		}
	})

	t.Run("target read scope", func(t *testing.T) {
		w := e2eGet(t, h, "/api/v1/targets?limit=100", env.tenantA)
		if w.Code != http.StatusOK {
			t.Fatalf("GET /targets = %d: %s", w.Code, w.Body)
		}
		if body := w.Body.String(); strings.Contains(body, "ext-b") || strings.Contains(body, env.extB.String()) {
			t.Errorf("scoped target list leaks plane B: %s", body)
		}
		if body := e2eGet(t, h, "/api/v1/targets?limit=100", env.globalAdmin).Body.String(); !strings.Contains(body, "ext-b") {
			t.Fatalf("global admin target list is missing plane B — absence assertion is vacuous: %s", body)
		}
		for _, suffix := range []string{"", "/series", "/stages", "/health", "/paths"} {
			path := "/api/v1/targets/" + env.extB.String() + suffix
			if w := e2eGet(t, h, path, env.tenantA); w.Code != http.StatusNotFound {
				t.Errorf("GET %s = %d, want 404: %s", path, w.Code, w.Body)
			}
		}
		if w := e2eGet(t, h, "/api/v1/targets/"+env.extA.String(), env.tenantA); w.Code != http.StatusOK {
			t.Errorf("own target summary = %d: %s", w.Code, w.Body)
		}
	})

	t.Run("enumeration reads hide the other plane", func(t *testing.T) {
		markers := []string{"tenant-b", "site-b1", "site-b2", env.agentB.String(), env.extB.String()}
		for _, path := range []string{
			"/api/v1/sites",
			"/api/v1/matrix",
			"/api/v1/outages",
			"/api/v1/path-events",
			"/api/v1/config/networks",
			"/api/v1/config/targets",
			"/api/v1/config/meshes",
			"/api/v1/config/probes",
			"/api/v1/config/tokens",
		} {
			w := e2eGet(t, h, path, env.tenantA)
			if w.Code != http.StatusOK {
				t.Errorf("GET %s = %d: %s", path, w.Code, w.Body)
				continue
			}
			body := w.Body.String()
			for _, marker := range markers {
				if strings.Contains(body, marker) {
					t.Errorf("GET %s leaks %q: %s", path, marker, body)
				}
			}
			// Anti-vacuity: the same read as global admin must surface at
			// least one plane-B marker, or the absence above proves nothing.
			admin := e2eGet(t, h, path, env.globalAdmin).Body.String()
			leaked := false
			for _, marker := range markers {
				if strings.Contains(admin, marker) {
					leaked = true
					break
				}
			}
			if !leaked {
				t.Errorf("global admin GET %s shows no plane-B marker — the scoped absence assertion is vacuous: %s", path, admin)
			}
		}
	})
}
