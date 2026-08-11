package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/devalexllc/polarbeam/internal/server/store"
)

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
		ID          string     `json:"id"`
		Username    string     `json:"username"`
		Role        string     `json:"role"`
		AuthSource  string     `json:"auth_source"`
		Status      string     `json:"status"`
		LoginCount  int64      `json:"login_count"`
		LastLoginAt *time.Time `json:"last_login_at"`
		CreatedAt   *time.Time `json:"created_at"`
	} `json:"users"`
	Total       int64 `json:"total"`
	LoginMonths []struct {
		Month       string `json:"month"`
		Total       int64  `json:"total"`
		Local       int64  `json:"local"`
		OIDC        int64  `json:"oidc"`
		UniqueUsers int64  `json:"unique_users"`
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
	f.userAccounts = []store.UserAccountInfo{
		{ID: uuid.New(), Username: "alice", Role: "admin", AuthSource: "local",
			Status: "active", CreatedAt: &created, LoginCount: 7, LastLoginAt: &lastLogin},
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
		{Month: time.Date(2025, 9, 1, 0, 0, 0, 0, time.UTC), Total: 42, Local: 30, OIDC: 12, UniqueUsers: 6},
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
	if bob.Status != "disabled" || bob.LoginCount != 0 {
		t.Errorf("bob = %+v", bob)
	}
	// Never-logged-in must serialize as an explicit null, not a zero time.
	if bob.LastLoginAt != nil {
		t.Errorf("bob last_login_at = %v, want null", bob.LastLoginAt)
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
	if sep.Month != "2025-09" || sep.Total != 42 || sep.Local != 30 || sep.OIDC != 12 || sep.UniqueUsers != 6 {
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
