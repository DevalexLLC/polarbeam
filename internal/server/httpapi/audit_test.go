package httpapi

// Audit contract tests: every mutating route emits exactly the one record
// the route table names; refused reads are recorded; identities and
// reasons ride on the records; secrets never do. These are the assertions
// docs/audit-logging.md promises operators.

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/devalexllc/polarbeam/internal/audit"
	"github.com/devalexllc/polarbeam/internal/server/oidcauth"
	"github.com/devalexllc/polarbeam/internal/server/store"
)

// expectedEvent is the record a mutating route owes. Everything not listed
// emits api.write; the two exceptions own a richer record and suppress
// the generic one. A new suppression must be declared here.
var expectedEvent = map[string]string{
	"POST /api/v1/auth/logout":    audit.EventSessionEnded,
	"PUT /api/v1/settings/syslog": audit.EventSyslogSettingsUpdate,
}

func routeEvent(route string) string {
	if ev, ok := expectedEvent[route]; ok {
		return ev
	}
	return audit.EventAPIWrite
}

func isMutating(route string) bool {
	return !strings.HasPrefix(route, "GET ") && strings.Contains(route, " ")
}

// TestAuditRouteContract: as admin, every session-guarded mutating route
// yields exactly one record of the expected event carrying route, path,
// status, actor, session, and remote; every 2xx read yields nothing.
func TestAuditRouteContract(t *testing.T) {
	f := newFakeDB()
	h, c := newTestAPIWithAudit(t, f)
	for _, route := range mountedRoutes(t) {
		class := routeDispositions[route]
		if class == "open" {
			continue
		}
		cookie, csrf := testSession(f, store.RoleAdmin, nil)
		before := len(c.records(""))
		w := probeRoute(h, route, cookie, csrf)
		recs := c.records("")[before:]
		if !isMutating(route) {
			if w.Code < 300 && len(recs) != 0 {
				t.Errorf("%s: successful read emitted %d records, want 0", route, len(recs))
			}
			continue
		}
		if len(recs) == 0 {
			t.Errorf("%s (%d): no audit record", route, w.Code)
			continue
		}
		// The route owes one record of its own event; session-ended
		// records for sessions it revoked may ride along. A route that owns
		// a richer success record still gets the generic one when it
		// refuses the request before reaching that point.
		want := routeEvent(route)
		if w.Code >= 400 {
			want = audit.EventAPIWrite
		}
		var own []slog.Record
		for _, r := range recs {
			if audit.EventID(r) == want {
				own = append(own, r)
			}
		}
		if len(own) != 1 {
			t.Errorf("%s (%d): %d records of %s, want exactly 1", route, w.Code, len(own), want)
			continue
		}
		r := own[0]
		if attr(r, "user") == "" || attr(r, "session") == "" || attr(r, "remote") != "203.0.113.9" {
			t.Errorf("%s: record lacks actor/session/remote: user=%q session=%q remote=%q", route, attr(r, "user"), attr(r, "session"), attr(r, "remote"))
		}
		if want == audit.EventAPIWrite {
			if attr(r, "route") != route || attr(r, "status") != fmt.Sprint(w.Code) {
				t.Errorf("%s: route=%q status=%q, want %q/%d", route, attr(r, "route"), attr(r, "status"), route, w.Code)
			}
		}
	}
}

// TestAuditPOSTIdentity: a create names the object it created — the
// collection route alone cannot.
func TestAuditPOSTIdentity(t *testing.T) {
	f := newFakeDB()
	h, c := newTestAPIWithAudit(t, f)
	cookie, csrf := configLogin(t, h, f, "admin")
	if _, err := f.CreateSite(t.Context(), "nyc", store.SiteUpdate{}); err != nil {
		t.Fatal(err)
	}
	f.targets = append(f.targets, store.TargetInfo{ID: uuid.New(), Name: "pg", Kind: "external", Network: ""})

	cases := []struct {
		path, body, key, want string
	}{
		{"/api/v1/config/targets", `{"name":"edge","address":"10.0.0.1"}`, "target", "edge"},
		{"/api/v1/config/meshes", `{"name":"m1"}`, "mesh", "m1"},
		{"/api/v1/config/probes", validDirectProbe, "probe_type", "tcp"},
		{"/api/v1/config/networks", `{"name":"tenant-b"}`, "network", "tenant-b"},
		{"/api/v1/config/sites", `{"name":"lon"}`, "site", "lon"},
		{"/api/v1/users", `{"username":"bob","role":"viewer"}`, "user_target", "bob"},
		{"/api/v1/config/tokens", `{"site":"nyc","ttl_ms":60000}`, "site", "nyc"},
	}
	for _, tc := range cases {
		before := len(c.records(audit.EventAPIWrite))
		w := doConfig(t, h, "POST", tc.path, tc.body, cookie, csrf)
		if w.Code != http.StatusOK {
			t.Fatalf("POST %s = %d: %s", tc.path, w.Code, w.Body)
		}
		recs := c.records(audit.EventAPIWrite)[before:]
		if len(recs) != 1 {
			t.Fatalf("POST %s: %d records", tc.path, len(recs))
		}
		if got := attr(recs[0], tc.key); got != tc.want {
			t.Errorf("POST %s: %s=%q, want %q", tc.path, tc.key, got, tc.want)
		}
		if tc.path == "/api/v1/config/probes" && attr(recs[0], "probe") == "" {
			t.Error("probe create record lacks the probe id")
		}
		if tc.path == "/api/v1/users" && attr(recs[0], "user_id") == "" {
			t.Error("user create record lacks user_id")
		}
	}
	// Two creates of different objects are distinguishable, and the join
	// token never appears in any record.
	var tok struct {
		Token string `json:"token"`
	}
	w := doConfig(t, h, "POST", "/api/v1/config/tokens", `{"site":"nyc","ttl_ms":60000}`, cookie, csrf)
	json.Unmarshal(w.Body.Bytes(), &tok)
	for _, r := range c.records("") {
		r.Attrs(func(a slog.Attr) bool {
			if strings.Contains(a.Value.String(), tok.Token) {
				t.Errorf("join token leaked into audit attr %q", a.Key)
			}
			return true
		})
	}
}

// TestAuditDenials: 401/403 and scope-404 on reads and writes, and the
// three session rejections, each with its reason.
func TestAuditDenials(t *testing.T) {
	f := newFakeDB()
	h, c := newTestAPIWithAudit(t, f)
	tenant := store.NetworkRef{ID: uuid.New(), Name: "tenant-a"}
	f.networks = append(f.networks, store.NetworkAdminInfo{ID: tenant.ID, Name: "tenant-a"})
	viewerC, viewerT := testSession(f, store.RoleViewer, nil)
	nviewC, _ := testSession(f, store.RoleNetworkViewer, []store.NetworkRef{tenant})
	nadmC, nadmT := testSession(f, store.RoleNetworkAdmin, []store.NetworkRef{tenant})

	last := func(id string) slog.Record {
		recs := c.records(id)
		if len(recs) == 0 {
			t.Fatalf("no %s record", id)
		}
		return recs[len(recs)-1]
	}

	// Viewer denied an admin read: authz.denied, 403.
	if w := doConfig(t, h, "GET", "/api/v1/users", "", viewerC, ""); w.Code != http.StatusForbidden {
		t.Fatalf("viewer GET /users = %d", w.Code)
	}
	if r := last(audit.EventAuthzDenied); attr(r, "outcome") != "denied" || attr(r, "status") != "403" || attr(r, "route") != "GET /api/v1/users" {
		t.Errorf("viewer denial record: outcome=%s status=%s route=%s", attr(r, "outcome"), attr(r, "status"), attr(r, "route"))
	}
	// Viewer denied a write: api.write, denied.
	doConfig(t, h, "POST", "/api/v1/config/networks", `{"name":"x"}`, viewerC, viewerT)
	if r := last(audit.EventAPIWrite); attr(r, "outcome") != "denied" || attr(r, "status") != "403" {
		t.Errorf("viewer write denial: outcome=%s status=%s", attr(r, "outcome"), attr(r, "status"))
	}
	// Scoped read of a target the store filtered out: 404 + reason.
	if w := doConfig(t, h, "GET", "/api/v1/config/targets/hidden", "", nviewC, ""); w.Code != http.StatusNotFound {
		t.Fatalf("scoped target GET = %d", w.Code)
	}
	if r := last(audit.EventAuthzDenied); attr(r, "reason") != "not_found_or_out_of_scope" || attr(r, "status") != "404" {
		t.Errorf("scoped 404 read: reason=%s status=%s", attr(r, "reason"), attr(r, "status"))
	}
	// Foreign site pair (nil endpoints) and foreign target detail.
	doConfig(t, h, "GET", "/api/v1/pairs/a/b", "", nviewC, "")
	if r := last(audit.EventAuthzDenied); attr(r, "reason") != "not_found_or_out_of_scope" {
		t.Errorf("pair read reason = %s", attr(r, "reason"))
	}
	doConfig(t, h, "GET", "/api/v1/targets/"+uuid.NewString(), "", nviewC, "")
	if r := last(audit.EventAuthzDenied); attr(r, "reason") != "not_found_or_out_of_scope" {
		t.Errorf("target detail reason = %s", attr(r, "reason"))
	}
	// Direct scope check on a write: out_of_scope, denied, response 404.
	if w := doConfig(t, h, "POST", "/api/v1/config/targets", `{"name":"x","address":"10.0.0.1","network":"default"}`, nadmC, nadmT); w.Code != http.StatusNotFound {
		t.Fatalf("foreign-plane write = %d", w.Code)
	}
	if r := last(audit.EventAPIWrite); attr(r, "reason") != "out_of_scope" || attr(r, "outcome") != "denied" {
		t.Errorf("foreign-plane write: reason=%s outcome=%s", attr(r, "reason"), attr(r, "outcome"))
	}

	// Session rejections.
	doConfig(t, h, "GET", "/api/v1/sites", "", nil, "")
	if r := last(audit.EventSessionRejected); attr(r, "reason") != "no_session_cookie" || attr(r, "route") != "GET /api/v1/sites" {
		t.Errorf("no cookie: reason=%s route=%s", attr(r, "reason"), attr(r, "route"))
	}
	doConfig(t, h, "GET", "/api/v1/sites", "", &http.Cookie{Name: sessionCookie, Value: "nope"}, "")
	if r := last(audit.EventSessionRejected); attr(r, "reason") != "unknown_or_expired_session" {
		t.Errorf("bad cookie reason = %s", attr(r, "reason"))
	}
	doConfig(t, h, "POST", "/api/v1/config/networks", `{"name":"x"}`, viewerC, "wrong")
	if r := last(audit.EventSessionRejected); attr(r, "reason") != "csrf" || attr(r, "user") == "" {
		t.Errorf("csrf: reason=%s user=%s", attr(r, "reason"), attr(r, "user"))
	}
}

// TestAuditLoginAndSessionLifecycle follows one session from login through
// a write to logout by its UUID, and records the failure reasons.
func TestAuditLoginAndSessionLifecycle(t *testing.T) {
	f := newFakeDB()
	h, c := newTestAPIWithAudit(t, f)
	f.addUser("alice", "hunter22222", "admin", false)
	f.addUser("dave", "hunter22222", "viewer", true)

	doLogin(t, h, "alice", "wrong-password")
	if r := c.records(audit.EventLogin)[0]; attr(r, "outcome") != "failure" || attr(r, "reason") != "invalid_credentials" || attr(r, "user") != "alice" || attr(r, "session") != "" || attr(r, "remote") != "203.0.113.7" {
		t.Errorf("bad password record: %s", recordString(r))
	}
	doLogin(t, h, "dave", "hunter22222")
	if r := c.records(audit.EventLogin)[1]; attr(r, "reason") != "disabled" {
		t.Errorf("disabled record: %s", recordString(r))
	}
	doLogin(t, h, "ghost", "hunter22222")
	if r := c.records(audit.EventLogin)[2]; attr(r, "reason") != "invalid_credentials" || attr(r, "user") != "ghost" {
		t.Errorf("unknown user record: %s", recordString(r))
	}

	w := doLogin(t, h, "alice", "hunter22222")
	if w.Code != http.StatusOK {
		t.Fatalf("login = %d", w.Code)
	}
	var res struct {
		CSRFToken string `json:"csrf_token"`
	}
	json.Unmarshal(w.Body.Bytes(), &res)
	cookie := w.Result().Cookies()[0]
	login := c.records(audit.EventLogin)[3]
	sid := attr(login, "session")
	if attr(login, "outcome") != "success" || attr(login, "user") != "alice" || attr(login, "user_source") != "local" || sid == "" {
		t.Fatalf("login success record: %s", recordString(login))
	}
	if _, err := uuid.Parse(sid); err != nil {
		t.Fatalf("session attr %q is not a UUID", sid)
	}

	doConfig(t, h, "POST", "/api/v1/config/networks", `{"name":"mgmt"}`, cookie, res.CSRFToken)
	if r := c.records(audit.EventAPIWrite)[0]; attr(r, "session") != sid {
		t.Errorf("write session = %s, want %s", attr(r, "session"), sid)
	}
	doConfig(t, h, "POST", "/api/v1/auth/logout", "", cookie, res.CSRFToken)
	ended := c.records(audit.EventSessionEnded)
	if len(ended) != 1 || attr(ended[0], "session") != sid || attr(ended[0], "reason") != "logout" || attr(ended[0], "user") != "alice" {
		t.Errorf("logout records = %d: %s", len(ended), recordString(ended[0]))
	}
	// Logout owns its record: no api.write for it.
	for _, r := range c.records(audit.EventAPIWrite) {
		if attr(r, "route") == "POST /api/v1/auth/logout" {
			t.Error("logout also emitted api.write")
		}
	}

	// Rate limit: the 11th attempt from one address is refused and recorded.
	for i := 0; i < loginLimit; i++ {
		doLogin(t, h, "alice", "wrong")
	}
	if w := doLogin(t, h, "alice", "hunter22222"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("11th login = %d, want 429", w.Code)
	}
	recs := c.records(audit.EventLogin)
	if r := recs[len(recs)-1]; attr(r, "outcome") != "denied" || attr(r, "reason") != "rate_limited" {
		t.Errorf("rate-limited record: %s", recordString(r))
	}
}

// TestAuditExpiredSessionsKeepTheirTime: cleanup days later records each
// expiry at its expiry time, with the cleanup time alongside.
func TestAuditExpiredSessionsKeepTheirTime(t *testing.T) {
	f := newFakeDB()
	h, c := newTestAPIWithAudit(t, f)
	f.addUser("alice", "hunter22222", "viewer", false)
	expiredAt := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	ref := store.SessionRef{ID: uuid.New(), UserID: uuid.New(), Username: "carol", ExpiresAt: expiredAt}
	f.expired = []store.SessionRef{ref}

	if w := doLogin(t, h, "alice", "hunter22222"); w.Code != http.StatusOK {
		t.Fatal(w.Code)
	}
	ended := c.records(audit.EventSessionEnded)
	if len(ended) != 1 {
		t.Fatalf("ended records = %d", len(ended))
	}
	r := ended[0]
	if !r.Time.Equal(expiredAt) {
		t.Errorf("record time = %v, want expiry %v", r.Time, expiredAt)
	}
	if attr(r, "reason") != "expired" || attr(r, "user") != "carol" || attr(r, "session") != ref.ID.String() || attr(r, "observed_at") == "" {
		t.Errorf("expired record: %s", recordString(r))
	}
}

// TestAuditPasswordChangeRevokes: the other session is recorded as ended
// and the write carries the count; the surviving one is not reported.
func TestAuditPasswordChangeRevokes(t *testing.T) {
	f := newFakeDB()
	h, c := newTestAPIWithAudit(t, f)
	cookieA, csrfA := loginRole(t, h, f, "alice", "viewer")
	doLogin(t, h, "alice", "hunter22222") // session B
	logins := c.records(audit.EventLogin)
	sidA, sidB := attr(logins[0], "session"), attr(logins[1], "session")

	if w := doConfig(t, h, "PUT", changePath, changeBody("hunter22222", "correct-horse-battery"), cookieA, csrfA); w.Code != http.StatusOK {
		t.Fatalf("change = %d: %s", w.Code, w.Body)
	}
	ended := c.records(audit.EventSessionEnded)
	if len(ended) != 1 || attr(ended[0], "session") != sidB || attr(ended[0], "reason") != "password_changed" {
		t.Errorf("ended = %d, want B only: %v", len(ended), sidA)
	}
	write := c.records(audit.EventAPIWrite)
	if r := write[len(write)-1]; attr(r, "sessions_revoked") != "1" || attr(r, "session") != sidA {
		t.Errorf("password change record: %s", recordString(r))
	}

	// Wrong current password: a failure with its reason, not a denial.
	doConfig(t, h, "PUT", changePath, changeBody("nope-nope-nope", "correct-horse-battery"), cookieA, csrfA)
	write = c.records(audit.EventAPIWrite)
	if r := write[len(write)-1]; attr(r, "outcome") != "failure" || attr(r, "reason") != "wrong_current_password" {
		t.Errorf("wrong current record: %s", recordString(r))
	}
}

// TestAuditRevocationSeparatesExpiredSessions: a password change's DELETE
// sweeps an already-expired row too; that session is recorded as expired
// at its expiry, not as revoked now.
func TestAuditRevocationSeparatesExpiredSessions(t *testing.T) {
	f := newFakeDB()
	h, c := newTestAPIWithAudit(t, f)
	cookieA, csrfA := loginRole(t, h, f, "alice", "viewer")
	alice := f.users["alice"]
	expiredAt := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	staleID, err := f.CreateSession(t.Context(), alice.ID, []byte("stale"), "csrf", expiredAt)
	if err != nil {
		t.Fatal(err)
	}
	if w := doConfig(t, h, "PUT", changePath, changeBody("hunter22222", "correct-horse-battery"), cookieA, csrfA); w.Code != http.StatusOK {
		t.Fatalf("change = %d: %s", w.Code, w.Body)
	}
	ended := c.records(audit.EventSessionEnded)
	if len(ended) != 1 || attr(ended[0], "session") != staleID.String() {
		t.Fatalf("ended = %d", len(ended))
	}
	if attr(ended[0], "reason") != "expired" || !ended[0].Time.Equal(expiredAt) || attr(ended[0], "observed_at") == "" {
		t.Errorf("swept expired session recorded as: %s at %v", recordString(ended[0]), ended[0].Time)
	}
}

// TestAuditUserLifecycle: disable ends live sessions, enable restores them,
// reset and delete revoke them, and every record names the account.
func TestAuditUserLifecycle(t *testing.T) {
	f := newFakeDB()
	h, c := newTestAPIWithAudit(t, f)
	cookie, csrf := configLogin(t, h, f, "admin")
	f.addUser("bob", "hunter22222", "viewer", false)
	bob := f.users["bob"]
	doLogin(t, h, "bob", "hunter22222")
	doLogin(t, h, "bob", "hunter22222")
	path := "/api/v1/users/" + bob.ID.String()

	doConfig(t, h, "PUT", path, `{"disabled":true}`, cookie, csrf)
	ended := c.records(audit.EventSessionEnded)
	if len(ended) != 2 || attr(ended[0], "reason") != "user_disabled" || attr(ended[0], "user") != "bob" {
		t.Errorf("disable ended = %d", len(ended))
	}
	w := c.records(audit.EventAPIWrite)
	if r := w[len(w)-1]; attr(r, "user_target") != "bob" || attr(r, "disabled") != "true" {
		t.Errorf("disable record: %s", recordString(r))
	}
	doConfig(t, h, "PUT", path, `{"disabled":false}`, cookie, csrf)
	if restored := c.records(audit.EventSessionRestored); len(restored) != 2 || attr(restored[0], "reason") != "user_enabled" {
		t.Errorf("restored = %d", len(restored))
	}
	// A repeated no-op enable (and disable of an already-disabled user)
	// records the write with changed=false and no session transitions.
	doConfig(t, h, "PUT", path, `{"disabled":false}`, cookie, csrf)
	if restored := c.records(audit.EventSessionRestored); len(restored) != 2 {
		t.Errorf("no-op enable restored = %d, want still 2", len(restored))
	}
	w = c.records(audit.EventAPIWrite)
	if r := w[len(w)-1]; attr(r, "changed") != "false" {
		t.Errorf("no-op enable record: %s", recordString(r))
	}
	doConfig(t, h, "POST", path+"/reset-password", "", cookie, csrf)
	w = c.records(audit.EventAPIWrite)
	if r := w[len(w)-1]; attr(r, "sessions_revoked") != "2" || attr(r, "user_target") != "bob" {
		t.Errorf("reset record: %s", recordString(r))
	}
	if ended := c.records(audit.EventSessionEnded); len(ended) != 4 || attr(ended[3], "reason") != "password_reset" {
		t.Errorf("after reset ended = %d", len(ended))
	}
	doLogin(t, h, "bob", "hunter22222") // fails: password reset; no session
	doConfig(t, h, "DELETE", path, "", cookie, csrf)
	w = c.records(audit.EventAPIWrite)
	if r := w[len(w)-1]; attr(r, "user_target") != "bob" || attr(r, "outcome") != "success" {
		t.Errorf("delete record: %s", recordString(r))
	}
}

// TestAuditOIDC: rate limits before the flow, account provisioning
// independent of session issuance, renames, and the login record.
func TestAuditOIDC(t *testing.T) {
	f := newFakeDB()
	f.oidcSettings = enabledSettings()
	tenant := store.NetworkAdminInfo{ID: uuid.New(), Name: "tenant-a"}
	f.networks = append(f.networks, tenant)
	fp := &fakeProviders{provider: &fakeProvider{
		claims: &oidcauth.Claims{Issuer: testIssuer, Subject: "sub-1", Username: "alice@corp", Role: "admin"},
	}, settings: f.oidcSettings}
	h, c := newTestAPIWithProvidersAudit(t, f, fp)

	// First login: created + login success with a session.
	if w := callback(t, h, "?code=authcode&state=st", stateCookie("st.n1.ver")); w.Code != http.StatusSeeOther {
		t.Fatalf("callback = %d", w.Code)
	}
	created := c.records(audit.EventSSOAccountCreated)
	if len(created) != 1 || attr(created[0], "user_target") != "alice@corp" || attr(created[0], "role") != "admin" || attr(created[0], "user_id") == "" {
		t.Fatalf("created records: %d", len(created))
	}
	login := c.records(audit.EventSSOLogin)
	if len(login) != 1 || attr(login[0], "outcome") != "success" || attr(login[0], "session") == "" || attr(login[0], "user_source") != "oidc" || attr(login[0], "issuer") != testIssuer {
		t.Fatalf("sso login record: %s", recordString(login[0]))
	}

	// Membership change: updated with previous role.
	fp.provider.claims = &oidcauth.Claims{Issuer: testIssuer, Subject: "sub-1", Username: "alice@corp", Role: store.RoleNetworkAdmin, Networks: []string{"tenant-a"}}
	callback(t, h, "?code=authcode&state=st", stateCookie("st.n1.ver"))
	updated := c.records(audit.EventSSOAccountUpdated)
	if len(updated) != 1 || attr(updated[0], "prev_role") != "admin" || attr(updated[0], "role") != store.RoleNetworkAdmin || attr(updated[0], "networks") != "tenant-a" {
		t.Fatalf("updated after role change: %d %s", len(updated), recordString(updated[0]))
	}
	// Rename only: updated with the previous username and the stable id.
	fp.provider.claims = &oidcauth.Claims{Issuer: testIssuer, Subject: "sub-1", Username: "alice.renamed", Role: store.RoleNetworkAdmin, Networks: []string{"tenant-a"}}
	callback(t, h, "?code=authcode&state=st", stateCookie("st.n1.ver"))
	updated = c.records(audit.EventSSOAccountUpdated)
	if len(updated) != 2 || attr(updated[1], "prev_username") != "alice@corp" || attr(updated[1], "user_target") != "alice.renamed" || attr(updated[1], "user_id") != attr(created[0], "user_id") {
		t.Fatalf("updated after rename: %s", recordString(updated[1]))
	}
	// Unchanged re-login: no account record.
	callback(t, h, "?code=authcode&state=st", stateCookie("st.n1.ver"))
	if len(c.records(audit.EventSSOAccountUpdated)) != 2 {
		t.Error("unchanged re-login emitted an account record")
	}

	// Session creation fails after the account write (provider switched
	// mid-flight): the account record, if any, still lands; the login is
	// a failure naming the identity.
	other := *f.oidcSettings // same revision, so the account write still commits
	other.ClientID = "someone-else"
	fp.settings = &other
	fp.provider.claims = &oidcauth.Claims{Issuer: testIssuer, Subject: "sub-1", Username: "alice.again", Role: store.RoleNetworkAdmin, Networks: []string{"tenant-a"}}
	callback(t, h, "?code=authcode&state=st", stateCookie("st.n1.ver"))
	if len(c.records(audit.EventSSOAccountUpdated)) != 3 {
		t.Error("account record missing when session creation failed")
	}
	login = c.records(audit.EventSSOLogin)
	if r := login[len(login)-1]; attr(r, "outcome") != "failure" || attr(r, "reason") != "provider_changed_during_login" || attr(r, "user") != "alice.again" {
		t.Errorf("failed login record: %s", recordString(r))
	}
	fp.settings = f.oidcSettings

	// Rate limits on start and callback: one record each, 429 unchanged.
	for i := 0; i < loginLimit; i++ {
		startFlow(t, h)
	}
	if w := startFlow(t, h); w.Code != http.StatusTooManyRequests {
		t.Fatalf("start after limit = %d", w.Code)
	}
	starts := c.records(audit.EventSSOStart)
	if r := starts[len(starts)-1]; attr(r, "reason") != "rate_limited" || attr(r, "outcome") != "denied" {
		t.Errorf("start rate-limit record: %s", recordString(r))
	}
	if w := callback(t, h, "?code=x&state=st", stateCookie("st.n1.ver")); w.Code != http.StatusTooManyRequests {
		t.Fatalf("callback after limit = %d", w.Code)
	}
	login = c.records(audit.EventSSOLogin)
	if r := login[len(login)-1]; attr(r, "reason") != "rate_limited" || attr(r, "remote") != "203.0.113.9" {
		t.Errorf("callback rate-limit record: %s", recordString(r))
	}
}

// TestAuditOIDCPolicyChangeNamesEverySession: a policy change ends the
// sessions of every federated user, each recorded by user and id.
func TestAuditOIDCPolicyChangeNamesEverySession(t *testing.T) {
	f := newFakeDB()
	f.oidcSettings = enabledSettings()
	h, c := newTestAPIWithAudit(t, f)
	cookie, csrf := adminAndCookie(t, h, f)
	var want []string
	for i := 1; i <= 3; i++ {
		u := f.addOIDCUser(testIssuer, fmt.Sprint("sub-", i), fmt.Sprint("fed", i), "viewer", false)
		id, err := f.CreateSession(t.Context(), u.ID, []byte(fmt.Sprint("hash", i)), "csrf", time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, id.String())
	}
	body := oidcSettingsBody()
	body["admin_values"] = []string{"new-admins"}
	if w := doSettings(t, h, "PUT", "/api/v1/settings/oidc", body, cookie, csrf); w.Code != http.StatusOK {
		t.Fatalf("PUT oidc = %d: %s", w.Code, w.Body)
	}
	ended := c.records(audit.EventSessionEnded)
	if len(ended) != 3 {
		t.Fatalf("ended = %d, want 3", len(ended))
	}
	seen := map[string]bool{}
	for _, r := range ended {
		if attr(r, "reason") != "oidc_policy_changed" || !strings.HasPrefix(attr(r, "user"), "fed") {
			t.Errorf("ended record: %s", recordString(r))
		}
		seen[attr(r, "session")] = true
	}
	for _, id := range want {
		if !seen[id] {
			t.Errorf("session %s not recorded as ended", id)
		}
	}
	writes := c.records(audit.EventAPIWrite)
	if r := writes[len(writes)-1]; attr(r, "sessions_revoked") != "3" || attr(r, "policy_changed") != "true" || attr(r, "provider_changed") != "false" {
		t.Errorf("oidc settings write record: %s", recordString(r))
	}
}

// TestAuditScoped404sCarryReason is the fence for the deliberately
// indistinguishable 404: every scoped route that answers 404 to a tenant
// principal must have marked the record with a reason, so a foreign plane
// is never recorded as a plain failure or (on a read) not at all.
func TestAuditScoped404sCarryReason(t *testing.T) {
	f := newFakeDB()
	h, c := newTestAPIWithAudit(t, f)
	tenant := store.NetworkRef{ID: uuid.New(), Name: "tenant-a"}
	f.networks = append(f.networks, store.NetworkAdminInfo{ID: tenant.ID, Name: "tenant-a"})
	nadmC, nadmT := testSession(f, store.RoleNetworkAdmin, []store.NetworkRef{tenant})
	nviewC, nviewT := testSession(f, store.RoleNetworkViewer, []store.NetworkRef{tenant})
	for _, route := range mountedRoutes(t) {
		class := routeDispositions[route]
		cookie, csrf := nviewC, nviewT
		switch class {
		case "scoped":
			cookie, csrf = nadmC, nadmT
		case "session":
		default:
			continue
		}
		before := len(c.records(""))
		w := probeRoute(h, route, cookie, csrf)
		if w.Code != http.StatusNotFound {
			continue
		}
		recs := c.records("")[before:]
		if len(recs) == 0 {
			t.Errorf("%s: 404 to a tenant with no audit record", route)
			continue
		}
		r := recs[len(recs)-1]
		switch reason := attr(r, "reason"); reason {
		case "out_of_scope", "not_found_or_out_of_scope":
			if attr(r, "outcome") != "denied" {
				t.Errorf("%s: scoped 404 outcome = %s, want denied", route, attr(r, "outcome"))
			}
		case "not_found":
			// A scope-blind lookup (probe PUT/DELETE pre-fetch): a genuine
			// miss, explicitly classified.
			if attr(r, "outcome") != "failure" {
				t.Errorf("%s: plain 404 outcome = %s, want failure", route, attr(r, "outcome"))
			}
		default:
			t.Errorf("%s: 404 recorded without a classification (reason=%q event=%s)", route, reason, audit.EventID(r))
		}
	}
}

// recordString renders a record as key=value pairs for failure messages.
func recordString(r slog.Record) string {
	var b strings.Builder
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
		return true
	})
	return b.String()
}

var _ = httptest.NewRecorder // keep the import stable for helpers above
