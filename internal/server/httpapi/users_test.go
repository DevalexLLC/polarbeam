package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/devalexllc/polarbeam/internal/server/auth"
	"github.com/devalexllc/polarbeam/internal/server/store"
)

// fakeUserState backs the userStore fake methods (which live in
// httpapi_test.go alongside the shared users/sessions maps they also touch).
type fakeUserState struct {
	userAccounts []store.UserAccountInfo
	loginMonths  []store.LoginMonthStat
	// beforeUpdateOwnPassword, when set, runs at the top of
	// UpdateOwnPassword — the seam for simulating an admin reset landing
	// between the handler's current-password verification and the store
	// transaction.
	beforeUpdateOwnPassword func()
}

func TestUsersAuth(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)

	// Account inventory is admin-only, like the token list.
	if w := doConfig(t, h, "GET", "/api/v1/users", "", nil, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("anon GET users = %d, want 401", w.Code)
	}
	viewerCookie, _ := configLogin(t, h, f, "viewer")
	if w := doConfig(t, h, "GET", "/api/v1/users", "", viewerCookie, ""); w.Code != http.StatusForbidden {
		t.Errorf("viewer GET users = %d, want 403: %s", w.Code, w.Body)
	}
	adminCookie, _ := configLogin(t, h, f, "admin")
	if w := doConfig(t, h, "GET", "/api/v1/users", "", adminCookie, ""); w.Code != http.StatusOK {
		t.Errorf("admin GET users = %d, want 200: %s", w.Code, w.Body)
	}
}

type usersResponse struct {
	Users []struct {
		ID           string     `json:"id"`
		Username     string     `json:"username"`
		Role         string     `json:"role"`
		AuthSource   string     `json:"auth_source"`
		Status       string     `json:"status"`
		LoginCount   int64      `json:"login_count"`
		LastLoginAt  *time.Time `json:"last_login_at"`
		LastActiveAt *time.Time `json:"last_active_at"`
		CreatedAt    *time.Time `json:"created_at"`
	} `json:"users"`
	Total       int64 `json:"total"`
	LoginMonths []struct {
		Month       string `json:"month"`
		Total       int64  `json:"total"`
		Local       int64  `json:"local"`
		OIDC        int64  `json:"oidc"`
		UniqueUsers int64  `json:"unique_users"`
		ActiveUsers int64  `json:"active_users"`
	} `json:"login_months"`
}

func getUsers(t *testing.T, h http.Handler, cookie *http.Cookie, query string) usersResponse {
	t.Helper()
	w := doConfig(t, h, "GET", "/api/v1/users"+query, "", cookie, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET users%s = %d: %s", query, w.Code, w.Body)
	}
	var res usersResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("bad body: %v: %s", err, w.Body)
	}
	return res
}

func seedUserAccounts(f *fakeDB) {
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	lastLogin := time.Date(2026, 8, 10, 12, 30, 0, 0, time.UTC)
	// Active after the last sign-in: the session outlived the month.
	lastActive := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	f.userAccounts = []store.UserAccountInfo{
		{ID: uuid.New(), Username: "alice", Role: "admin", AuthSource: "local",
			Status: "active", CreatedAt: &created, LoginCount: 7, LastLoginAt: &lastLogin,
			LastActiveAt: &lastActive},
		{ID: uuid.New(), Username: "bob", Role: "viewer", AuthSource: "oidc",
			Status: "disabled", CreatedAt: &created},
		// A deleted identity, reconstructed from login-event snapshots:
		// no users row, so no created time.
		{ID: uuid.New(), Username: "carol", Role: "viewer", AuthSource: "oidc",
			Status: "deleted", LoginCount: 3, LastLoginAt: &lastLogin},
	}
}

func TestUsersGetShape(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	seedUserAccounts(f)
	f.loginMonths = []store.LoginMonthStat{
		{Month: time.Date(2025, 9, 1, 0, 0, 0, 0, time.UTC), Total: 42, Local: 30, OIDC: 12, UniqueUsers: 6, ActiveUsers: 9},
		{Month: time.Date(2025, 10, 1, 0, 0, 0, 0, time.UTC)},
	}
	cookie, _ := configLogin(t, h, f, "admin")

	res := getUsers(t, h, cookie, "")
	if len(res.Users) != 3 || res.Total != 3 {
		t.Fatalf("users = %d total %d, want 3/3", len(res.Users), res.Total)
	}
	alice, bob, carol := res.Users[0], res.Users[1], res.Users[2]
	if alice.Username != "alice" || alice.Role != "admin" || alice.AuthSource != "local" ||
		alice.Status != "active" || alice.LoginCount != 7 || alice.LastLoginAt == nil || alice.CreatedAt == nil {
		t.Errorf("alice = %+v", alice)
	}
	if alice.LastActiveAt == nil || !alice.LastActiveAt.After(*alice.LastLoginAt) {
		t.Errorf("alice last_active_at = %v, want after last sign-in", alice.LastActiveAt)
	}
	if bob.Status != "disabled" || bob.LoginCount != 0 {
		t.Errorf("bob = %+v", bob)
	}
	// Never-logged-in must serialize as an explicit null, not a zero time.
	if bob.LastLoginAt != nil || bob.LastActiveAt != nil {
		t.Errorf("bob last_login_at = %v last_active_at = %v, want null", bob.LastLoginAt, bob.LastActiveAt)
	}
	// Deleted identities keep counts and last-known fields but have no
	// users row, hence a null created_at.
	if carol.Status != "deleted" || carol.LoginCount != 3 || carol.CreatedAt != nil || carol.Role != "viewer" {
		t.Errorf("carol = %+v", carol)
	}

	if len(res.LoginMonths) != 2 {
		t.Fatalf("login_months = %d, want 2", len(res.LoginMonths))
	}
	sep := res.LoginMonths[0]
	if sep.Month != "2025-09" || sep.Total != 42 || sep.Local != 30 || sep.OIDC != 12 || sep.UniqueUsers != 6 || sep.ActiveUsers != 9 {
		t.Errorf("september bucket = %+v", sep)
	}
	if empty := res.LoginMonths[1]; empty.Month != "2025-10" || empty.Total != 0 {
		t.Errorf("zero-filled bucket = %+v", empty)
	}
}

func TestUsersFilters(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	seedUserAccounts(f)
	cookie, _ := configLogin(t, h, f, "admin")

	cases := []struct {
		query string
		want  []string
	}{
		{"?q=ali", []string{"alice"}},
		{"?role=viewer", []string{"bob", "carol"}},
		{"?status=deleted", []string{"carol"}},
		{"?source=oidc", []string{"bob", "carol"}},
		{"?role=viewer&source=oidc&status=disabled", []string{"bob"}},
		{"?q=nobody", nil},
	}
	for _, c := range cases {
		res := getUsers(t, h, cookie, c.query)
		var got []string
		for _, u := range res.Users {
			got = append(got, u.Username)
		}
		if len(got) != len(c.want) || res.Total != int64(len(c.want)) {
			t.Errorf("%s -> %v total %d, want %v", c.query, got, res.Total, c.want)
			continue
		}
		for i := range c.want {
			if got[i] != c.want[i] {
				t.Errorf("%s -> %v, want %v", c.query, got, c.want)
				break
			}
		}
	}
}

func TestUsersPagination(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	seedUserAccounts(f)
	cookie, _ := configLogin(t, h, f, "admin")

	res := getUsers(t, h, cookie, "?limit=2")
	if len(res.Users) != 2 || res.Total != 3 {
		t.Errorf("limit=2 -> %d users total %d, want 2/3", len(res.Users), res.Total)
	}
	res = getUsers(t, h, cookie, "?limit=2&offset=2")
	if len(res.Users) != 1 || res.Total != 3 || res.Users[0].Username != "carol" {
		t.Errorf("offset=2 -> %+v total %d, want just carol of 3", res.Users, res.Total)
	}
	// Past the end: empty window, real total.
	res = getUsers(t, h, cookie, "?limit=2&offset=10")
	if len(res.Users) != 0 || res.Total != 3 {
		t.Errorf("offset=10 -> %d users total %d, want 0/3", len(res.Users), res.Total)
	}
}

func TestUsersParamValidation(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, _ := configLogin(t, h, f, "admin")

	bad := []string{
		"?role=root",
		"?status=gone",
		"?source=ldap",
		"?limit=0",
		"?limit=9999",
		"?limit=abc",
		"?offset=-1",
	}
	for _, q := range bad {
		if w := doConfig(t, h, "GET", "/api/v1/users"+q, "", cookie, ""); w.Code != http.StatusBadRequest {
			t.Errorf("GET users%s = %d, want 400: %s", q, w.Code, w.Body)
		}
	}
	// Every problem reported at once.
	w := doConfig(t, h, "GET", "/api/v1/users?role=root&limit=abc", "", cookie, "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("combined bad params = %d, want 400", w.Code)
	}
	msg := errBody(t, w)
	for _, want := range []string{"role must be", "limit must be"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
}

func TestUserCreate(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, csrf := configLogin(t, h, f, "admin")

	// Viewer and CSRF-less requests are rejected before the handler.
	viewerCookie, viewerCSRF := configLogin(t, h, f, "viewer")
	if w := doConfig(t, h, "POST", "/api/v1/users", `{"username":"dave","role":"viewer"}`, viewerCookie, viewerCSRF); w.Code != http.StatusForbidden {
		t.Errorf("viewer create = %d, want 403", w.Code)
	}
	if w := doConfig(t, h, "POST", "/api/v1/users", `{"username":"dave","role":"viewer"}`, cookie, ""); w.Code != http.StatusForbidden {
		t.Errorf("create without CSRF = %d, want 403", w.Code)
	}

	w := doConfig(t, h, "POST", "/api/v1/users", `{"username":"dave","role":"viewer"}`, cookie, csrf)
	if w.Code != http.StatusOK {
		t.Fatalf("create = %d: %s", w.Code, w.Body)
	}
	var created struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Role     string `json:"role"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("bad body: %v", err)
	}
	if created.Username != "dave" || created.Role != "viewer" || len(created.Password) < 20 {
		t.Errorf("created = %+v", created)
	}
	// The generated password is real: it verifies against the stored hash,
	// and the cleartext was never persisted.
	u := f.users["dave"]
	if u == nil {
		t.Fatal("dave not stored")
	}
	if ok, err := auth.VerifyPassword(created.Password, u.PasswordHash); err != nil || !ok {
		t.Errorf("generated password does not verify: ok=%v err=%v", ok, err)
	}

	if w := doConfig(t, h, "POST", "/api/v1/users", `{"username":"dave","role":"viewer"}`, cookie, csrf); w.Code != http.StatusConflict {
		t.Errorf("duplicate create = %d, want 409: %s", w.Code, w.Body)
	}
	if w := doConfig(t, h, "POST", "/api/v1/users", `{"username":"","role":"root"}`, cookie, csrf); w.Code != http.StatusBadRequest {
		t.Errorf("invalid create = %d, want 400", w.Code)
	} else {
		msg := errBody(t, w)
		for _, want := range []string{"username is required", "role must be"} {
			if !strings.Contains(msg, want) {
				t.Errorf("error %q missing %q", msg, want)
			}
		}
	}
	// Unknown fields are client bugs, never dropped.
	if w := doConfig(t, h, "POST", "/api/v1/users", `{"username":"eve","role":"viewer","password":"mine"}`, cookie, csrf); w.Code != http.StatusBadRequest {
		t.Errorf("unknown field = %d, want 400", w.Code)
	}
}

func TestUserDisableAndDelete(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, csrf := configLogin(t, h, f, "admin")
	f.addUser("bob", "hunter22222", "viewer", false)
	bob := f.users["bob"]
	self := f.users["user-admin"]

	// Disable bob, then re-enable him.
	if w := doConfig(t, h, "PUT", "/api/v1/users/"+bob.ID.String(), `{"disabled":true}`, cookie, csrf); w.Code != http.StatusOK {
		t.Fatalf("disable bob = %d: %s", w.Code, w.Body)
	}
	if !f.users["bob"].Disabled {
		t.Error("bob not disabled")
	}
	if w := doConfig(t, h, "PUT", "/api/v1/users/"+bob.ID.String(), `{"disabled":false}`, cookie, csrf); w.Code != http.StatusOK {
		t.Fatalf("enable bob = %d: %s", w.Code, w.Body)
	}
	if f.users["bob"].Disabled {
		t.Error("bob still disabled")
	}

	// Self-service lockout is refused before the store is consulted.
	if w := doConfig(t, h, "PUT", "/api/v1/users/"+self.ID.String(), `{"disabled":true}`, cookie, csrf); w.Code != http.StatusConflict {
		t.Errorf("self disable = %d, want 409: %s", w.Code, w.Body)
	}
	if w := doConfig(t, h, "DELETE", "/api/v1/users/"+self.ID.String(), "", cookie, csrf); w.Code != http.StatusConflict {
		t.Errorf("self delete = %d, want 409: %s", w.Code, w.Body)
	}

	// Validation.
	if w := doConfig(t, h, "PUT", "/api/v1/users/not-a-uuid", `{"disabled":true}`, cookie, csrf); w.Code != http.StatusBadRequest {
		t.Errorf("bad uuid = %d, want 400", w.Code)
	}
	if w := doConfig(t, h, "PUT", "/api/v1/users/"+bob.ID.String(), `{}`, cookie, csrf); w.Code != http.StatusBadRequest {
		t.Errorf("missing disabled = %d, want 400", w.Code)
	}
	if w := doConfig(t, h, "PUT", "/api/v1/users/"+uuid.New().String(), `{"disabled":true}`, cookie, csrf); w.Code != http.StatusNotFound {
		t.Errorf("unknown id disable = %d, want 404", w.Code)
	}

	// Delete bob; his row is gone, a second delete 404s.
	if w := doConfig(t, h, "DELETE", "/api/v1/users/"+bob.ID.String(), "", cookie, csrf); w.Code != http.StatusOK {
		t.Fatalf("delete bob = %d: %s", w.Code, w.Body)
	}
	if f.users["bob"] != nil {
		t.Error("bob still present after delete")
	}
	if w := doConfig(t, h, "DELETE", "/api/v1/users/"+bob.ID.String(), "", cookie, csrf); w.Code != http.StatusNotFound {
		t.Errorf("second delete = %d, want 404", w.Code)
	}
}

// sessionCountFor counts stored sessions belonging to a user.
func (f *fakeDB) sessionCountFor(id uuid.UUID) int {
	n := 0
	for _, s := range f.sessions {
		if s.UserID == id {
			n++
		}
	}
	return n
}

func TestUserResetPassword(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, csrf := configLogin(t, h, f, "admin")
	f.addUser("bob", "hunter22222", "viewer", false)
	bob := f.users["bob"]
	oldHash := bob.PasswordHash
	self := f.users["user-admin"]

	// Give bob a live session so the reset has something to invalidate.
	if w := doLogin(t, h, "bob", "hunter22222"); w.Code != http.StatusOK {
		t.Fatalf("bob login = %d: %s", w.Code, w.Body)
	}
	if f.sessionCountFor(bob.ID) != 1 {
		t.Fatalf("bob sessions = %d, want 1", f.sessionCountFor(bob.ID))
	}

	// Viewer and CSRF-less requests are rejected before the handler.
	viewerCookie, viewerCSRF := configLogin(t, h, f, "viewer")
	if w := doConfig(t, h, "POST", "/api/v1/users/"+bob.ID.String()+"/reset-password", "", viewerCookie, viewerCSRF); w.Code != http.StatusForbidden {
		t.Errorf("viewer reset = %d, want 403", w.Code)
	}
	if w := doConfig(t, h, "POST", "/api/v1/users/"+bob.ID.String()+"/reset-password", "", cookie, ""); w.Code != http.StatusForbidden {
		t.Errorf("reset without CSRF = %d, want 403", w.Code)
	}
	if f.sessionCountFor(bob.ID) != 1 {
		t.Fatalf("rejected resets must not touch sessions")
	}

	w := doConfig(t, h, "POST", "/api/v1/users/"+bob.ID.String()+"/reset-password", "", cookie, csrf)
	if w.Code != http.StatusOK {
		t.Fatalf("reset = %d: %s", w.Code, w.Body)
	}
	var reset struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Role     string `json:"role"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &reset); err != nil {
		t.Fatalf("bad body: %v", err)
	}
	if reset.ID != bob.ID.String() || reset.Username != "bob" || reset.Role != "viewer" || len(reset.Password) < 20 {
		t.Errorf("reset = %+v", reset)
	}
	if ok, err := auth.VerifyPassword(reset.Password, bob.PasswordHash); err != nil || !ok {
		t.Errorf("reset password does not verify: ok=%v err=%v", ok, err)
	}
	if bob.PasswordHash == oldHash {
		t.Error("hash unchanged by reset")
	}
	// The old credential is dead on both fronts: sessions and logins.
	if f.sessionCountFor(bob.ID) != 0 {
		t.Errorf("bob sessions after reset = %d, want 0", f.sessionCountFor(bob.ID))
	}
	if w := doLogin(t, h, "bob", "hunter22222"); w.Code != http.StatusUnauthorized {
		t.Errorf("old-password login after reset = %d, want 401", w.Code)
	}
	if w := doLogin(t, h, "bob", reset.Password); w.Code != http.StatusOK {
		t.Errorf("new-password login = %d: %s", w.Code, w.Body)
	}

	// Reset leaves the disabled flag alone: enable is a separate lever.
	bob.Disabled = true
	if w := doConfig(t, h, "POST", "/api/v1/users/"+bob.ID.String()+"/reset-password", "", cookie, csrf); w.Code != http.StatusOK {
		t.Fatalf("reset of disabled bob = %d: %s", w.Code, w.Body)
	}
	if !bob.Disabled {
		t.Error("reset must not re-enable a disabled account")
	}

	// Self-reset is refused: it would kill the caller's own session.
	if w := doConfig(t, h, "POST", "/api/v1/users/"+self.ID.String()+"/reset-password", "", cookie, csrf); w.Code != http.StatusConflict {
		t.Errorf("self reset = %d, want 409: %s", w.Code, w.Body)
	} else if msg := errBody(t, w); !strings.Contains(msg, "user menu") {
		t.Errorf("self-reset error %q should point at the user menu", msg)
	}

	// Bad UUID, unknown id (the deleted-identity case: login_events rows
	// have no users row), and federated accounts — all explicit refusals.
	if w := doConfig(t, h, "POST", "/api/v1/users/not-a-uuid/reset-password", "", cookie, csrf); w.Code != http.StatusBadRequest {
		t.Errorf("bad uuid reset = %d, want 400", w.Code)
	}
	if w := doConfig(t, h, "POST", "/api/v1/users/"+uuid.New().String()+"/reset-password", "", cookie, csrf); w.Code != http.StatusNotFound {
		t.Errorf("unknown id reset = %d, want 404: %s", w.Code, w.Body)
	}
	fed := f.addOIDCUser("https://idp.example", "sub-1", "fed", "viewer", false)
	if w := doConfig(t, h, "POST", "/api/v1/users/"+fed.ID.String()+"/reset-password", "", cookie, csrf); w.Code != http.StatusConflict {
		t.Errorf("federated reset = %d, want 409: %s", w.Code, w.Body)
	} else if msg := errBody(t, w); !strings.Contains(msg, "identity provider") {
		t.Errorf("federated-reset error %q should name the identity provider", msg)
	}
}

func TestLoginRecordsEvent(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	f.addUser("alice", "hunter22222", "admin", false)

	if w := doLogin(t, h, "alice", "wrong-password"); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad login = %d, want 401", w.Code)
	}
	if len(f.logins) != 0 {
		t.Fatalf("failed login recorded %d event(s), want 0", len(f.logins))
	}

	if w := doLogin(t, h, "alice", "hunter22222"); w.Code != http.StatusOK {
		t.Fatalf("login = %d: %s", w.Code, w.Body)
	}
	if len(f.logins) != 1 {
		t.Fatalf("recorded %d event(s), want 1", len(f.logins))
	}
	got := f.logins[0]
	if got.Username != "alice" || got.Role != "admin" || got.AuthSource != "local" || got.UserID != f.users["alice"].ID {
		t.Errorf("recorded login = %+v", got)
	}
}
