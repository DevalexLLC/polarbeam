package httpapi

// fakeDB network methods live here, next to the handlers they serve (the
// per-feature convention: siteconfig_test.go holds site+token fakes, etc.).

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

func (f *fakeDB) NetworkIDByName(_ context.Context, name string) (uuid.UUID, error) {
	for _, n := range f.networks {
		if n.Name == name {
			return n.ID, nil
		}
	}
	return uuid.Nil, fmt.Errorf("network %q does not exist%w", name, store.ErrNotFound)
}

func (f *fakeDB) ListNetworksConfig(_ context.Context) ([]store.NetworkAdminInfo, error) {
	// Recompute reference counts from the other fake slices so tests seed
	// plain rows and the counts stay honest, like the store's subqueries.
	out := make([]store.NetworkAdminInfo, len(f.networks))
	for i, n := range f.networks {
		out[i] = n
		out[i].AgentCount, out[i].TokenCount, out[i].MeshCount, out[i].ProbeCount = f.networkRefs(n.Name)
	}
	return out, nil
}

func (f *fakeDB) networkRefs(name string) (agents, tokens, meshes, probes int64) {
	for _, a := range f.agents {
		if a.Network == name {
			agents++
		}
	}
	for _, t := range f.joinTokens {
		if t.Network == name {
			tokens++
		}
	}
	for _, m := range f.meshes {
		if m.Network == name {
			meshes++
		}
	}
	for _, p := range f.probes {
		if p.Site != "" && p.Network == name {
			probes++
		}
	}
	return
}

func (f *fakeDB) CreateNetwork(_ context.Context, name, displayName string) (uuid.UUID, error) {
	for _, n := range f.networks {
		if n.Name == name {
			return uuid.Nil, fmt.Errorf("network %q already exists%w", name, store.ErrConflict)
		}
	}
	n := store.NetworkAdminInfo{ID: uuid.New(), Name: name, DisplayName: displayName, CreatedAt: time.Now()}
	f.networks = append(f.networks, n)
	return n.ID, nil
}

func (f *fakeDB) UpdateNetwork(_ context.Context, name, displayName string) error {
	for i := range f.networks {
		if f.networks[i].Name == name {
			f.networks[i].DisplayName = displayName
			return nil
		}
	}
	return fmt.Errorf("network %q does not exist%w", name, store.ErrNotFound)
}

func (f *fakeDB) DeleteNetwork(_ context.Context, name string) (int64, error) {
	if name == "default" {
		return 0, fmt.Errorf("the 'default' network cannot be deleted (it is the seeded fallback for enrollment and admin writes)%w", store.ErrInvalid)
	}
	idx := -1
	for i, n := range f.networks {
		if n.Name == name {
			idx = i
		}
	}
	if idx < 0 {
		return 0, fmt.Errorf("network %q does not exist%w", name, store.ErrNotFound)
	}
	// Mirror the store: unused tokens sweep with the network; anything else
	// (or a used token) blocks the delete.
	var kept []store.JoinTokenInfo
	var swept int64
	for _, t := range f.joinTokens {
		if t.Network == name && t.UsedAt == nil {
			swept++
			continue
		}
		kept = append(kept, t)
	}
	agents, tokens, meshes, probes := f.networkRefs(name)
	tokens -= swept
	if agents > 0 || meshes > 0 || probes > 0 || tokens > 0 {
		return 0, fmt.Errorf("network %q is referenced by %d agent(s), %d mesh group(s), %d probe config(s), and %d join token(s)%w",
			name, agents, meshes, probes, tokens, store.ErrConflict)
	}
	f.joinTokens = kept
	f.networks = append(f.networks[:idx], f.networks[idx+1:]...)
	return swept, nil
}

func TestNetworkConfigAuth(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	viewerCookie, viewerCSRF := configLogin(t, h, f, "viewer")

	// Network reads are any-session — topology vocabulary, like sites.
	if w := doConfig(t, h, "GET", "/api/v1/config/networks", "", nil, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("anon GET networks = %d, want 401", w.Code)
	}
	if w := doConfig(t, h, "GET", "/api/v1/config/networks", "", viewerCookie, ""); w.Code != http.StatusOK {
		t.Errorf("viewer GET networks = %d, want 200: %s", w.Code, w.Body)
	}

	writes := []struct{ method, path, body string }{
		{"POST", "/api/v1/config/networks", `{"name":"x"}`},
		{"PUT", "/api/v1/config/networks/x", `{"display_name":"X"}`},
		{"DELETE", "/api/v1/config/networks/x", ""},
	}
	for _, wr := range writes {
		if w := doConfig(t, h, wr.method, wr.path, wr.body, nil, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("anon %s %s = %d, want 401", wr.method, wr.path, w.Code)
		}
		if w := doConfig(t, h, wr.method, wr.path, wr.body, viewerCookie, viewerCSRF); w.Code != http.StatusForbidden {
			t.Errorf("viewer %s %s = %d, want 403", wr.method, wr.path, w.Code)
		}
	}

	adminCookie, _ := configLogin(t, h, f, "admin")
	if w := doConfig(t, h, "POST", "/api/v1/config/networks", `{"name":"x"}`, adminCookie, ""); w.Code != http.StatusForbidden {
		t.Errorf("admin POST without CSRF = %d, want 403", w.Code)
	}
}

func TestNetworkConfigLifecycle(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, csrf := configLogin(t, h, f, "admin")

	// Validation: empty name, and unknown fields are client bugs.
	w := doConfig(t, h, "POST", "/api/v1/config/networks", `{"name":"  "}`, cookie, csrf)
	if w.Code != http.StatusBadRequest || !strings.Contains(errBody(t, w), "name is required") {
		t.Errorf("empty name = %d %q, want 400 naming name", w.Code, w.Body)
	}
	w = doConfig(t, h, "POST", "/api/v1/config/networks", `{"network":"mgmt"}`, cookie, csrf)
	if w.Code != http.StatusBadRequest {
		t.Errorf("unknown field = %d, want 400", w.Code)
	}

	// Create, then list with the seeded default first (sorted upstream; the
	// fake preserves insertion order: default, mgmt).
	w = doConfig(t, h, "POST", "/api/v1/config/networks", `{"name":"mgmt","display_name":"Management"}`, cookie, csrf)
	if w.Code != http.StatusOK {
		t.Fatalf("POST network = %d: %s", w.Code, w.Body)
	}
	w = doConfig(t, h, "POST", "/api/v1/config/networks", `{"name":"mgmt"}`, cookie, csrf)
	if w.Code != http.StatusConflict {
		t.Errorf("duplicate network = %d, want 409", w.Code)
	}
	var list struct {
		Networks []networkConfigJSON `json:"networks"`
	}
	w = doConfig(t, h, "GET", "/api/v1/config/networks", "", cookie, "")
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("list body %q: %v", w.Body, err)
	}
	if len(list.Networks) != 2 || list.Networks[1].Name != "mgmt" || list.Networks[1].DisplayName != "Management" {
		t.Fatalf("list = %+v, want default + mgmt", list.Networks)
	}

	// Display name is updatable; the name is not.
	w = doConfig(t, h, "PUT", "/api/v1/config/networks/mgmt", `{"display_name":"Management plane"}`, cookie, csrf)
	if w.Code != http.StatusOK {
		t.Errorf("PUT display_name = %d: %s", w.Code, w.Body)
	}
	w = doConfig(t, h, "PUT", "/api/v1/config/networks/mgmt", `{"name":"mgmt2","display_name":"x"}`, cookie, csrf)
	if w.Code != http.StatusBadRequest || !strings.Contains(errBody(t, w), "cannot be changed in place") {
		t.Errorf("rename attempt = %d %q, want 400", w.Code, w.Body)
	}
	w = doConfig(t, h, "PUT", "/api/v1/config/networks/nope", `{"display_name":"x"}`, cookie, csrf)
	if w.Code != http.StatusNotFound {
		t.Errorf("PUT unknown network = %d, want 404", w.Code)
	}

	// Delete: referenced → 409; 'default' → 400; unknown → 404; the
	// unused-token sweep count rides the success response.
	f.agents = append(f.agents, store.AgentListInfo{ID: uuid.New(), Site: "site-a", Network: "mgmt", Hostname: "a1"})
	w = doConfig(t, h, "DELETE", "/api/v1/config/networks/mgmt", "", cookie, csrf)
	if w.Code != http.StatusConflict || !strings.Contains(errBody(t, w), "1 agent(s)") {
		t.Errorf("delete referenced = %d %q, want 409 naming counts", w.Code, w.Body)
	}
	w = doConfig(t, h, "DELETE", "/api/v1/config/networks/default", "", cookie, csrf)
	if w.Code != http.StatusBadRequest {
		t.Errorf("delete default = %d, want 400", w.Code)
	}
	w = doConfig(t, h, "DELETE", "/api/v1/config/networks/nope", "", cookie, csrf)
	if w.Code != http.StatusNotFound {
		t.Errorf("delete unknown = %d, want 404", w.Code)
	}
	f.agents = nil
	f.joinTokens = append(f.joinTokens, store.JoinTokenInfo{ID: uuid.New(), Site: "site-a", Network: "mgmt"})
	w = doConfig(t, h, "DELETE", "/api/v1/config/networks/mgmt", "", cookie, csrf)
	if w.Code != http.StatusOK {
		t.Fatalf("delete mgmt = %d: %s", w.Code, w.Body)
	}
	var del struct {
		TokensDeleted int64 `json:"tokens_deleted"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &del); err != nil || del.TokensDeleted != 1 {
		t.Errorf("delete response %q (err %v), want tokens_deleted 1", w.Body, err)
	}
}
