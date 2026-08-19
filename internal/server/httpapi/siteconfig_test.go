package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/devalexllc/polarbeam/internal/server/store"
)

// --- fakeDB implementation of the site/token config methods ---

func (f *fakeDB) ListSitesConfig(_ context.Context) ([]store.SiteAdminInfo, error) {
	return f.siteConfigs, nil
}

func (f *fakeDB) CreateSite(_ context.Context, name string, up store.SiteUpdate) (uuid.UUID, error) {
	for _, s := range f.siteConfigs {
		if s.Name == name {
			return uuid.Nil, fmt.Errorf("site %q already exists%w", name, store.ErrConflict)
		}
	}
	si := store.SiteAdminInfo{CreatedAt: time.Now()}
	si.ID, si.Name = uuid.New(), name
	if up.DisplayName != nil {
		si.DisplayName = *up.DisplayName
	}
	if up.Location != nil {
		si.Location = *up.Location
	}
	si.Latitude, si.Longitude = up.Latitude, up.Longitude
	f.siteConfigs = append(f.siteConfigs, si)
	return si.ID, nil
}

func (f *fakeDB) UpdateSite(_ context.Context, name string, up store.SiteUpdate) error {
	for i := range f.siteConfigs {
		if f.siteConfigs[i].Name != name {
			continue
		}
		if up.DisplayName != nil {
			f.siteConfigs[i].DisplayName = *up.DisplayName
		}
		if up.Location != nil {
			f.siteConfigs[i].Location = *up.Location
		}
		if up.ClearCoords {
			f.siteConfigs[i].Latitude, f.siteConfigs[i].Longitude = nil, nil
		} else if up.Latitude != nil {
			f.siteConfigs[i].Latitude, f.siteConfigs[i].Longitude = up.Latitude, up.Longitude
		}
		return nil
	}
	return fmt.Errorf("site %q does not exist%w", name, store.ErrNotFound)
}

func (f *fakeDB) DeleteSite(_ context.Context, name string) (int64, error) {
	for i, s := range f.siteConfigs {
		if s.Name != name {
			continue
		}
		// Mirror the store: unused tokens are swept with the site; any
		// other token (used, or raced in) joins the conflict counts.
		var deletable, remaining int64
		for _, t := range f.joinTokens {
			if t.Site != name {
				continue
			}
			if t.UsedAt == nil {
				deletable++
			} else {
				remaining++
			}
		}
		if s.AgentCount > 0 || s.MeshCount > 0 || s.ProbeCount > 0 || remaining > 0 {
			return 0, fmt.Errorf("site %q is referenced by %d agent(s), %d mesh membership(s), %d probe config(s), and %d join token(s)%w",
				name, s.AgentCount, s.MeshCount, s.ProbeCount, remaining, store.ErrConflict)
		}
		kept := f.joinTokens[:0]
		for _, t := range f.joinTokens {
			if t.Site != name {
				kept = append(kept, t)
			}
		}
		f.joinTokens = kept
		f.siteConfigs = append(f.siteConfigs[:i], f.siteConfigs[i+1:]...)
		return deletable, nil
	}
	return 0, fmt.Errorf("site %q does not exist%w", name, store.ErrNotFound)
}

func (f *fakeDB) SiteIDByName(_ context.Context, name string) (uuid.UUID, error) {
	for _, s := range f.siteConfigs {
		if s.Name == name {
			return s.ID, nil
		}
	}
	return uuid.Nil, fmt.Errorf("site %q does not exist%w", name, store.ErrNotFound)
}

func (f *fakeDB) ListJoinTokens(_ context.Context) ([]store.JoinTokenInfo, error) {
	return f.joinTokens, nil
}

func (f *fakeDB) CreateJoinToken(_ context.Context, siteID, networkID uuid.UUID, createdBy string, ttl time.Duration) (string, error) {
	site := ""
	for _, s := range f.siteConfigs {
		if s.ID == siteID {
			site = s.Name
		}
	}
	network := ""
	for _, n := range f.networks {
		if n.ID == networkID {
			network = n.Name
		}
	}
	t := store.JoinTokenInfo{
		ID: uuid.New(), Site: site, Network: network, CreatedBy: createdBy,
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(ttl),
	}
	f.joinTokens = append(f.joinTokens, t)
	return t.ID.String() + ".fixed-secret", nil
}

func (f *fakeDB) DeleteJoinToken(_ context.Context, id uuid.UUID) error {
	for i, t := range f.joinTokens {
		if t.ID != id {
			continue
		}
		if t.UsedAt != nil {
			return fmt.Errorf("join token %s was used to enroll an agent and is kept as an audit record%w", id, store.ErrConflict)
		}
		f.joinTokens = append(f.joinTokens[:i], f.joinTokens[i+1:]...)
		return nil
	}
	return fmt.Errorf("join token %s does not exist%w", id, store.ErrNotFound)
}

// addSiteConfig seeds a site row with reference counts.
func (f *fakeDB) addSiteConfig(name string, agents, meshes, probes int64) store.SiteAdminInfo {
	si := store.SiteAdminInfo{AgentCount: agents, MeshCount: meshes, ProbeCount: probes, CreatedAt: time.Now()}
	si.ID, si.Name = uuid.New(), name
	f.siteConfigs = append(f.siteConfigs, si)
	return si
}

// --- tests ---

func TestSiteConfigAuth(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	viewerCookie, viewerCSRF := configLogin(t, h, f, "viewer")

	// Site reads are any-session, like the other config reads.
	if w := doConfig(t, h, "GET", "/api/v1/config/sites", "", nil, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("anon GET sites = %d, want 401", w.Code)
	}
	if w := doConfig(t, h, "GET", "/api/v1/config/sites", "", viewerCookie, ""); w.Code != http.StatusOK {
		t.Errorf("viewer GET sites = %d, want 200: %s", w.Code, w.Body)
	}

	// The token LIST is admin-only — the load-bearing difference from every
	// other config read.
	if w := doConfig(t, h, "GET", "/api/v1/config/tokens", "", nil, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("anon GET tokens = %d, want 401", w.Code)
	}
	if w := doConfig(t, h, "GET", "/api/v1/config/tokens", "", viewerCookie, ""); w.Code != http.StatusForbidden {
		t.Errorf("viewer GET tokens = %d, want 403: %s", w.Code, w.Body)
	}

	writes := []struct{ method, path, body string }{
		{"POST", "/api/v1/config/sites", `{"name":"x"}`},
		{"PUT", "/api/v1/config/sites/x", `{"name":"x"}`},
		{"DELETE", "/api/v1/config/sites/x", ""},
		{"POST", "/api/v1/config/tokens", `{"site":"x","ttl_ms":1000}`},
		{"DELETE", "/api/v1/config/tokens/" + uuid.Nil.String(), ""},
	}
	for _, wr := range writes {
		if w := doConfig(t, h, wr.method, wr.path, wr.body, nil, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("anon %s %s = %d, want 401", wr.method, wr.path, w.Code)
		}
		if w := doConfig(t, h, wr.method, wr.path, wr.body, viewerCookie, viewerCSRF); w.Code != http.StatusForbidden {
			t.Errorf("viewer %s %s = %d, want 403", wr.method, wr.path, w.Code)
		}
	}

	// Admin without the CSRF header is rejected before the handler.
	adminCookie, _ := configLogin(t, h, f, "admin")
	if w := doConfig(t, h, "POST", "/api/v1/config/tokens", `{"site":"x","ttl_ms":1000}`, adminCookie, ""); w.Code != http.StatusForbidden {
		t.Errorf("admin token POST without CSRF = %d, want 403", w.Code)
	}
}

func TestSiteConfigCreateValidation(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, csrf := configLogin(t, h, f, "admin")

	// Every problem reported at once.
	w := doConfig(t, h, "POST", "/api/v1/config/sites", `{"name":"","latitude":91,"longitude":-181}`, cookie, csrf)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("POST invalid site = %d, want 400: %s", w.Code, w.Body)
	}
	msg := errBody(t, w)
	for _, want := range []string{"name is required", "latitude must be", "longitude must be"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}

	// Coordinates are both-or-neither.
	w = doConfig(t, h, "POST", "/api/v1/config/sites", `{"name":"nyc","latitude":40.7}`, cookie, csrf)
	if w.Code != http.StatusBadRequest || !strings.Contains(errBody(t, w), "set together") {
		t.Errorf("lat-without-lon = %d %q, want 400 naming both-or-neither", w.Code, w.Body)
	}

	// Unknown fields are client bugs, never dropped.
	w = doConfig(t, h, "POST", "/api/v1/config/sites", `{"name":"nyc","lat":40.7}`, cookie, csrf)
	if w.Code != http.StatusBadRequest {
		t.Errorf("unknown field = %d, want 400", w.Code)
	}

	// A valid create, 0,0 being a real coordinate.
	w = doConfig(t, h, "POST", "/api/v1/config/sites", `{"name":"gulf","display_name":"Gulf of Guinea","location":"","latitude":0,"longitude":0}`, cookie, csrf)
	if w.Code != http.StatusOK {
		t.Fatalf("POST valid site = %d: %s", w.Code, w.Body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("create response %q: err=%v", w.Body, err)
	}

	// Duplicate names conflict rather than silently adopting the row.
	w = doConfig(t, h, "POST", "/api/v1/config/sites", `{"name":"gulf"}`, cookie, csrf)
	if w.Code != http.StatusConflict {
		t.Errorf("duplicate site = %d, want 409: %s", w.Code, w.Body)
	}

	// The list carries the created coords.
	w = doConfig(t, h, "GET", "/api/v1/config/sites", "", cookie, "")
	var list struct {
		Sites []struct {
			Name      string   `json:"name"`
			Latitude  *float64 `json:"latitude"`
			Longitude *float64 `json:"longitude"`
		} `json:"sites"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("list body %q: %v", w.Body, err)
	}
	if len(list.Sites) != 1 || list.Sites[0].Latitude == nil || *list.Sites[0].Latitude != 0 {
		t.Errorf("list = %+v, want gulf at 0,0", list.Sites)
	}
}

func TestSiteConfigPut(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, csrf := configLogin(t, h, f, "admin")

	lat, lon := 51.5, -0.13
	si := f.addSiteConfig("lon", 0, 0, 0)
	for i := range f.siteConfigs {
		if f.siteConfigs[i].ID == si.ID {
			f.siteConfigs[i].Latitude, f.siteConfigs[i].Longitude = &lat, &lon
		}
	}

	// Full-state update keeps coords when both are sent.
	w := doConfig(t, h, "PUT", "/api/v1/config/sites/lon", `{"name":"lon","display_name":"London","location":"London, UK","latitude":51.5074,"longitude":-0.1278}`, cookie, csrf)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", w.Code, w.Body)
	}
	if f.siteConfigs[0].DisplayName != "London" || f.siteConfigs[0].Latitude == nil || *f.siteConfigs[0].Latitude != 51.5074 {
		t.Errorf("after PUT: %+v", f.siteConfigs[0])
	}

	// Omitting both coordinates clears them (full-state semantics).
	w = doConfig(t, h, "PUT", "/api/v1/config/sites/lon", `{"name":"lon","display_name":"London","location":"London, UK"}`, cookie, csrf)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT clear = %d: %s", w.Code, w.Body)
	}
	if f.siteConfigs[0].Latitude != nil || f.siteConfigs[0].Longitude != nil {
		t.Errorf("coords not cleared: %+v", f.siteConfigs[0])
	}

	// One-sided coordinates stay invalid on PUT too.
	w = doConfig(t, h, "PUT", "/api/v1/config/sites/lon", `{"name":"lon","longitude":-0.1278}`, cookie, csrf)
	if w.Code != http.StatusBadRequest || !strings.Contains(errBody(t, w), "set together") {
		t.Errorf("one-sided PUT = %d %q, want 400", w.Code, w.Body)
	}

	// Name is identity.
	w = doConfig(t, h, "PUT", "/api/v1/config/sites/lon", `{"name":"london"}`, cookie, csrf)
	if w.Code != http.StatusBadRequest || !strings.Contains(errBody(t, w), "cannot be changed in place") {
		t.Errorf("rename = %d %q, want 400", w.Code, w.Body)
	}

	if w := doConfig(t, h, "PUT", "/api/v1/config/sites/nope", `{"name":"nope"}`, cookie, csrf); w.Code != http.StatusNotFound {
		t.Errorf("PUT unknown = %d, want 404", w.Code)
	}
}

func TestSiteConfigDelete(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, csrf := configLogin(t, h, f, "admin")

	f.addSiteConfig("busy", 2, 1, 3)
	empty := f.addSiteConfig("empty", 0, 0, 0)
	f.joinTokens = append(f.joinTokens, store.JoinTokenInfo{
		ID: uuid.New(), Site: "empty", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	})
	_ = empty

	// Referenced sites are refused with every blocking count named.
	w := doConfig(t, h, "DELETE", "/api/v1/config/sites/busy", "", cookie, csrf)
	if w.Code != http.StatusConflict {
		t.Fatalf("DELETE busy = %d, want 409: %s", w.Code, w.Body)
	}
	msg := errBody(t, w)
	for _, want := range []string{"2 agent(s)", "1 mesh membership(s)", "3 probe config(s)"} {
		if !strings.Contains(msg, want) {
			t.Errorf("conflict %q missing %q", msg, want)
		}
	}

	// An unreferenced site deletes, taking its unused tokens with it —
	// reported, never silent.
	w = doConfig(t, h, "DELETE", "/api/v1/config/sites/empty", "", cookie, csrf)
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE empty = %d: %s", w.Code, w.Body)
	}
	var res struct {
		TokensDeleted int64 `json:"tokens_deleted"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.TokensDeleted != 1 {
		t.Errorf("delete response %q, want tokens_deleted=1", w.Body)
	}
	if len(f.joinTokens) != 0 {
		t.Errorf("tokens remain: %+v", f.joinTokens)
	}

	if w := doConfig(t, h, "DELETE", "/api/v1/config/sites/nope", "", cookie, csrf); w.Code != http.StatusNotFound {
		t.Errorf("DELETE unknown = %d, want 404", w.Code)
	}

	// A token the sweep cannot delete (used, or committed in the race
	// window) blocks the site delete as a conflict, never an FK 500.
	f.addSiteConfig("tokonly", 0, 0, 0)
	usedAt := time.Now()
	f.joinTokens = append(f.joinTokens, store.JoinTokenInfo{
		ID: uuid.New(), Site: "tokonly", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), UsedAt: &usedAt,
	})
	w = doConfig(t, h, "DELETE", "/api/v1/config/sites/tokonly", "", cookie, csrf)
	if w.Code != http.StatusConflict || !strings.Contains(errBody(t, w), "1 join token(s)") {
		t.Errorf("DELETE site with surviving token = %d %q, want 409 naming the token", w.Code, w.Body)
	}
}

func TestTokenLifecycle(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, csrf := configLogin(t, h, f, "admin")
	f.addSiteConfig("nyc", 1, 0, 0)

	// Create: the cleartext appears exactly once, in this response.
	w := doConfig(t, h, "POST", "/api/v1/config/tokens", `{"site":"nyc","ttl_ms":3600000}`, cookie, csrf)
	if w.Code != http.StatusOK {
		t.Fatalf("POST token = %d: %s", w.Code, w.Body)
	}
	var created struct {
		Token     string    `json:"token"`
		Site      string    `json:"site"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("create body %q: %v", w.Body, err)
	}
	if !strings.Contains(created.Token, ".") || created.Site != "nyc" || created.ExpiresAt.IsZero() {
		t.Errorf("create response = %+v", created)
	}
	// created_by is the session username, like probe updated_by.
	if got := f.joinTokens[0].CreatedBy; got != "user-admin" {
		t.Errorf("created_by = %q, want session username", got)
	}

	if w := doConfig(t, h, "POST", "/api/v1/config/tokens", `{"site":"nope","ttl_ms":1000}`, cookie, csrf); w.Code != http.StatusNotFound {
		t.Errorf("POST unknown site = %d, want 404 (never auto-created)", w.Code)
	}
	// ttl_ms is bounded above too: past MaxInt64/1e6 the ms→Duration
	// multiplication would wrap.
	for _, body := range []string{
		`{"site":"nyc","ttl_ms":0}`, `{"site":"nyc","ttl_ms":-5}`, `{"site":"","ttl_ms":1000}`,
		`{"site":"nyc","ttl_ms":9223372036855}`,
	} {
		if w := doConfig(t, h, "POST", "/api/v1/config/tokens", body, cookie, csrf); w.Code != http.StatusBadRequest {
			t.Errorf("POST %s = %d, want 400", body, w.Code)
		}
	}

	// The list surfaces used/unused shape for the SPA's derived status.
	usedAt := time.Now()
	agentID := uuid.New()
	hostname := "edge-1"
	f.joinTokens = append(f.joinTokens, store.JoinTokenInfo{
		ID: uuid.New(), Site: "nyc", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
		UsedAt: &usedAt, UsedByAgent: &agentID, UsedByHostname: &hostname,
	})
	w = doConfig(t, h, "GET", "/api/v1/config/tokens", "", cookie, "")
	var list struct {
		Tokens []struct {
			ID             string     `json:"id"`
			Site           string     `json:"site"`
			UsedAt         *time.Time `json:"used_at"`
			UsedByHostname *string    `json:"used_by_hostname"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Tokens) != 2 {
		t.Fatalf("list %q: %v", w.Body, err)
	}
	if list.Tokens[0].UsedAt != nil {
		t.Errorf("unused token carries used_at: %+v", list.Tokens[0])
	}
	if list.Tokens[1].UsedAt == nil || list.Tokens[1].UsedByHostname == nil || *list.Tokens[1].UsedByHostname != "edge-1" {
		t.Errorf("used token shape: %+v", list.Tokens[1])
	}

	// Delete = revoke, unused only.
	unusedID, usedID := f.joinTokens[0].ID, f.joinTokens[1].ID
	if w := doConfig(t, h, "DELETE", "/api/v1/config/tokens/"+unusedID.String(), "", cookie, csrf); w.Code != http.StatusOK {
		t.Errorf("DELETE unused = %d: %s", w.Code, w.Body)
	}
	w = doConfig(t, h, "DELETE", "/api/v1/config/tokens/"+usedID.String(), "", cookie, csrf)
	if w.Code != http.StatusConflict || !strings.Contains(errBody(t, w), "audit record") {
		t.Errorf("DELETE used = %d %q, want 409", w.Code, w.Body)
	}
	if w := doConfig(t, h, "DELETE", "/api/v1/config/tokens/"+uuid.New().String(), "", cookie, csrf); w.Code != http.StatusNotFound {
		t.Errorf("DELETE unknown = %d, want 404", w.Code)
	}
	if w := doConfig(t, h, "DELETE", "/api/v1/config/tokens/not-a-uuid", "", cookie, csrf); w.Code != http.StatusBadRequest {
		t.Errorf("DELETE bad id = %d, want 400", w.Code)
	}
}
