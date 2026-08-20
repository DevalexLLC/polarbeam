package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
	"github.com/devalexllc/polarbeam/internal/server/store"
)

// --- fakeDB implementation of the config methods ---

func (f *fakeDB) ListTargets(_ context.Context, networks []uuid.UUID) ([]store.TargetInfo, error) {
	f.recordScope("ListTargets", networks)
	return f.targets, nil
}

func (f *fakeDB) UpsertExternalTarget(_ context.Context, name, address string, port int32, url string) (uuid.UUID, error) {
	for i := range f.targets {
		if f.targets[i].Name == name {
			if f.targets[i].Kind != "external" {
				return uuid.Nil, fmt.Errorf("target %q already exists as an agent target%w", name, store.ErrConflict)
			}
			f.targets[i].Address, f.targets[i].Port, f.targets[i].URL = address, port, url
			return f.targets[i].ID, nil
		}
	}
	t := store.TargetInfo{ID: uuid.New(), Kind: "external", Name: name, Address: address, Port: port, URL: url}
	f.targets = append(f.targets, t)
	return t.ID, nil
}

func (f *fakeDB) DeleteTarget(_ context.Context, name string) error {
	for i, t := range f.targets {
		if t.Name == name && t.Kind == "external" {
			if t.ProbeCount > 0 {
				return store.InUseError{Name: name, Count: t.ProbeCount}
			}
			f.targets = append(f.targets[:i], f.targets[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("external target %q does not exist%w", name, store.ErrNotFound)
}

func (f *fakeDB) ListMeshGroups(_ context.Context, networks []uuid.UUID) ([]store.MeshGroupInfo, error) {
	f.recordScope("ListMeshGroups", networks)
	return f.meshes, nil
}

func (f *fakeDB) UpsertMeshGroup(_ context.Context, name string, networkID *uuid.UUID) (uuid.UUID, error) {
	// Mirror the store: nil = no opinion (create on default, keep existing);
	// explicit mismatch with an existing mesh is a conflict.
	network := "default"
	if networkID != nil {
		network = ""
		for _, n := range f.networks {
			if n.ID == *networkID {
				network = n.Name
			}
		}
	}
	for _, m := range f.meshes {
		if m.Name == name {
			if networkID != nil && m.Network != network {
				return uuid.Nil, fmt.Errorf("mesh %q already exists on another network (a mesh's network cannot be changed; delete and re-create it)%w", name, store.ErrConflict)
			}
			return m.ID, nil
		}
	}
	m := store.MeshGroupInfo{ID: uuid.New(), Name: name, Network: network}
	f.meshes = append(f.meshes, m)
	return m.ID, nil
}

func (f *fakeDB) DeleteMeshGroup(_ context.Context, name string) (int64, error) {
	for i, m := range f.meshes {
		if m.Name == name {
			f.meshes = append(f.meshes[:i], f.meshes[i+1:]...)
			return m.ProbeCount, nil
		}
	}
	return 0, fmt.Errorf("mesh group %q does not exist%w", name, store.ErrNotFound)
}

func (f *fakeDB) AddMeshMember(_ context.Context, meshName, siteName string) error {
	for i := range f.meshes {
		if f.meshes[i].Name == meshName {
			f.meshes[i].Sites = append(f.meshes[i].Sites, siteName)
			return nil
		}
	}
	return fmt.Errorf("mesh group %q does not exist%w", meshName, store.ErrNotFound)
}

func (f *fakeDB) RemoveMeshMember(_ context.Context, meshName, siteName string) error {
	for i := range f.meshes {
		if f.meshes[i].Name != meshName {
			continue
		}
		for j, s := range f.meshes[i].Sites {
			if s == siteName {
				f.meshes[i].Sites = append(f.meshes[i].Sites[:j], f.meshes[i].Sites[j+1:]...)
				return nil
			}
		}
		return fmt.Errorf("site %q is not a member of mesh %q%w", siteName, meshName, store.ErrNotFound)
	}
	return fmt.Errorf("mesh group %q does not exist%w", meshName, store.ErrNotFound)
}

func (f *fakeDB) ListProbeConfigs(_ context.Context, networks []uuid.UUID) ([]store.ProbeConfigInfo, error) {
	f.recordScope("ListProbeConfigs", networks)
	return f.probes, nil
}

func (f *fakeDB) GetProbeConfig(_ context.Context, id uuid.UUID) (*store.ProbeConfigInfo, error) {
	for i := range f.probes {
		if f.probes[i].ID == id {
			p := f.probes[i]
			return &p, nil
		}
	}
	return nil, fmt.Errorf("probe config %s does not exist%w", id, store.ErrNotFound)
}

func (f *fakeDB) AddDirectProbe(_ context.Context, siteName, targetName string, networkID uuid.UUID, ps store.ProbeSettings, enabled bool, updatedBy string) (uuid.UUID, error) {
	// Mirror the store's rule: agent-kind targets cannot take direct probes.
	for _, t := range f.targets {
		if t.Name == targetName && t.Kind == "agent" {
			return uuid.Nil, fmt.Errorf("target %q is an enrollment-managed agent target: direct probes need an external target (mesh probes cover agent peers)%w", targetName, store.ErrInvalid)
		}
	}
	network := ""
	for _, n := range f.networks {
		if n.ID == networkID {
			network = n.Name
		}
	}
	p := store.ProbeConfigInfo{
		ID: uuid.New(), Site: siteName, Target: targetName, Network: network, ProbeType: ps.ProbeType,
		Interval: ps.Interval, Timeout: ps.Timeout, TrainCount: ps.TrainCount,
		TrainSpacing: ps.TrainSpacing, Params: ps.Params, Enabled: enabled, UpdatedBy: updatedBy,
	}
	f.probes = append(f.probes, p)
	return p.ID, nil
}

func (f *fakeDB) AddMeshProbe(_ context.Context, meshName string, ps store.ProbeSettings, enabled bool, updatedBy string) (uuid.UUID, error) {
	p := store.ProbeConfigInfo{
		ID: uuid.New(), Mesh: meshName, ProbeType: ps.ProbeType,
		Interval: ps.Interval, Timeout: ps.Timeout, TrainCount: ps.TrainCount,
		TrainSpacing: ps.TrainSpacing, Params: ps.Params, Enabled: enabled, UpdatedBy: updatedBy,
	}
	f.probes = append(f.probes, p)
	return p.ID, nil
}

func (f *fakeDB) UpdateProbeConfig(_ context.Context, id uuid.UUID, ps store.ProbeSettings, enabled bool, updatedBy string) error {
	for i := range f.probes {
		if f.probes[i].ID == id {
			f.probes[i].Interval, f.probes[i].Timeout = ps.Interval, ps.Timeout
			f.probes[i].TrainCount, f.probes[i].TrainSpacing = ps.TrainCount, ps.TrainSpacing
			f.probes[i].Params, f.probes[i].Enabled = ps.Params, enabled
			f.probes[i].UpdatedBy = updatedBy
			return nil
		}
	}
	return fmt.Errorf("probe config %s does not exist%w", id, store.ErrNotFound)
}

func (f *fakeDB) DeleteProbeConfig(_ context.Context, id uuid.UUID) error {
	for i := range f.probes {
		if f.probes[i].ID == id {
			f.probes = append(f.probes[:i], f.probes[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("probe config %s does not exist%w", id, store.ErrNotFound)
}

// --- helpers ---

// configLogin wraps settings_test.go's loginRole with a per-role username.
func configLogin(t *testing.T, h http.Handler, f *fakeDB, role string) (*http.Cookie, string) {
	t.Helper()
	return loginRole(t, h, f, "user-"+role, role)
}

func doConfig(t *testing.T, h http.Handler, method, path, body string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *bytes.Reader
	if body == "" {
		rd = bytes.NewReader(nil)
	} else {
		rd = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(method, path, rd)
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

func errBody(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("non-JSON error body: %q", w.Body)
	}
	return body.Error
}

const validDirectProbe = `{"site":"nyc","target":"pg","type":"tcp","interval_ms":10000,"timeout_ms":5000,"train_count":0,"train_spacing_ms":0,"params":{}}`

// --- tests ---

func TestConfigAuth(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	viewerCookie, viewerCSRF := configLogin(t, h, f, "viewer")

	reads := []string{
		"/api/v1/config/probe-types", "/api/v1/config/targets",
		"/api/v1/config/meshes", "/api/v1/config/probes",
	}
	for _, path := range reads {
		if w := doConfig(t, h, "GET", path, "", nil, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("anon GET %s = %d, want 401", path, w.Code)
		}
		if w := doConfig(t, h, "GET", path, "", viewerCookie, ""); w.Code != http.StatusOK {
			t.Errorf("viewer GET %s = %d, want 200: %s", path, w.Code, w.Body)
		}
	}

	writes := []struct{ method, path, body string }{
		{"POST", "/api/v1/config/targets", `{"name":"x","address":"y"}`},
		{"DELETE", "/api/v1/config/targets/x", ""},
		{"POST", "/api/v1/config/meshes", `{"name":"m"}`},
		{"DELETE", "/api/v1/config/meshes/m", ""},
		{"POST", "/api/v1/config/meshes/m/members/nyc", ""},
		{"DELETE", "/api/v1/config/meshes/m/members/nyc", ""},
		{"POST", "/api/v1/config/probes", validDirectProbe},
		{"PUT", "/api/v1/config/probes/" + uuid.Nil.String(), validDirectProbe},
		{"DELETE", "/api/v1/config/probes/" + uuid.Nil.String(), ""},
	}
	for _, wr := range writes {
		if w := doConfig(t, h, wr.method, wr.path, wr.body, nil, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("anon %s %s = %d, want 401", wr.method, wr.path, w.Code)
		}
		if w := doConfig(t, h, wr.method, wr.path, wr.body, viewerCookie, viewerCSRF); w.Code != http.StatusForbidden {
			t.Errorf("viewer %s %s = %d, want 403", wr.method, wr.path, w.Code)
		}
	}

	// Admin without the CSRF header must be rejected before the handler.
	adminCookie, _ := configLogin(t, h, f, "admin")
	if w := doConfig(t, h, "POST", "/api/v1/config/meshes", `{"name":"m"}`, adminCookie, ""); w.Code != http.StatusForbidden {
		t.Errorf("admin write without CSRF = %d, want 403", w.Code)
	}
}

// TestConfigBodyTooLarge proves the body-limit middleware wraps the
// authenticated admin routes too, via decodeStrict's 413 mapping.
func TestConfigBodyTooLarge(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, csrf := configLogin(t, h, f, "admin")

	body := `{"name":"` + strings.Repeat("a", maxRequestBody) + `"}`
	w := doConfig(t, h, "POST", "/api/v1/config/targets", body, cookie, csrf)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized target body = %d, want 413: %s", w.Code, w.Body)
	}
}

func TestConfigTargetValidationAndConflict(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, csrf := configLogin(t, h, f, "admin")

	// Every problem reported at once.
	w := doConfig(t, h, "POST", "/api/v1/config/targets", `{"name":"","port":70000}`, cookie, csrf)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid target = %d, want 400: %s", w.Code, w.Body)
	}
	msg := errBody(t, w)
	for _, want := range []string{"name is required", "address or url is required", "port must be between"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}

	// Unknown JSON field and trailing data are client bugs, not no-ops.
	for _, body := range []string{`{"name":"x","address":"y","bogus":1}`, `{"name":"x","address":"y"} {}`} {
		if w := doConfig(t, h, "POST", "/api/v1/config/targets", body, cookie, csrf); w.Code != http.StatusBadRequest {
			t.Errorf("body %q = %d, want 400", body, w.Code)
		}
	}

	// In-use target deletes are 409 with the count; unknown targets 404.
	f.targets = append(f.targets, store.TargetInfo{ID: uuid.New(), Kind: "external", Name: "pg", ProbeCount: 3})
	w = doConfig(t, h, "DELETE", "/api/v1/config/targets/pg", "", cookie, csrf)
	if w.Code != http.StatusConflict {
		t.Fatalf("in-use delete = %d, want 409: %s", w.Code, w.Body)
	}
	if msg := errBody(t, w); !strings.Contains(msg, "3 probe config(s)") {
		t.Errorf("409 message %q must carry the count", msg)
	}
	if w := doConfig(t, h, "DELETE", "/api/v1/config/targets/nope", "", cookie, csrf); w.Code != http.StatusNotFound {
		t.Errorf("unknown delete = %d, want 404", w.Code)
	}
}

func TestConfigProbeValidation(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, csrf := configLogin(t, h, f, "admin")

	cases := []struct {
		name, body   string
		wantContains string
	}{
		{"unknown type", `{"site":"a","target":"b","type":"smtp","interval_ms":10000,"timeout_ms":5000}`,
			`unknown probe type "smtp"`},
		{"mesh and site both", `{"site":"a","target":"b","mesh":"m","type":"tcp","interval_ms":10000,"timeout_ms":5000}`,
			"exactly one of mesh or site+target"},
		{"neither", `{"type":"tcp","interval_ms":10000,"timeout_ms":5000}`,
			"exactly one of mesh or site+target"},
		{"timeout >= interval", `{"site":"a","target":"b","type":"tcp","interval_ms":5000,"timeout_ms":5000}`,
			"timeout_ms (5s) must be shorter than interval_ms (5s)"},
		{"train too long", `{"site":"a","target":"b","type":"icmp","interval_ms":30000,"timeout_ms":2000,"train_count":20}`,
			"must fit inside timeout_ms"},
		{"unknown param key", `{"site":"a","target":"b","type":"icmp","interval_ms":10000,"timeout_ms":5000,"params":{"bogus":"1"}}`,
			`unknown key "bogus" for probe type icmp`},
		{"port on direct probe", `{"site":"a","target":"b","type":"tcp","interval_ms":10000,"timeout_ms":5000,"params":{"port":"9"}}`,
			`"port" applies only to mesh probes`},
		{"mesh tcp needs port", `{"mesh":"m","type":"tcp","interval_ms":10000,"timeout_ms":5000}`,
			`"port" is required for mesh tcp probes`},
		{"http mesh rejected", `{"mesh":"m","type":"http","interval_ms":10000,"timeout_ms":5000}`,
			"http probes cannot be mesh templates"},
		{"ntp mesh rejected", `{"mesh":"m","type":"ntp","interval_ms":90000,"timeout_ms":5000}`,
			"ntp probes cannot be mesh templates"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := doConfig(t, h, "POST", "/api/v1/config/probes", c.body, cookie, csrf)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("code = %d, want 400: %s", w.Code, w.Body)
			}
			if msg := errBody(t, w); !strings.Contains(msg, c.wantContains) {
				t.Errorf("error %q missing %q", msg, c.wantContains)
			}
		})
	}

	// A direct probe against an agent-kind target is a 400, not a 500.
	f.targets = append(f.targets, store.TargetInfo{ID: uuid.New(), Kind: "agent", Name: "agent:peer"})
	w := doConfig(t, h, "POST", "/api/v1/config/probes",
		`{"site":"nyc","target":"agent:peer","type":"tcp","interval_ms":10000,"timeout_ms":5000}`, cookie, csrf)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("agent-target direct probe = %d, want 400: %s", w.Code, w.Body)
	}
	if msg := errBody(t, w); !strings.Contains(msg, "enrollment-managed agent target") {
		t.Errorf("error %q must explain the agent-target rejection", msg)
	}

	// A valid create lands with the session username as updated_by.
	w = doConfig(t, h, "POST", "/api/v1/config/probes", validDirectProbe, cookie, csrf)
	if w.Code != http.StatusOK {
		t.Fatalf("valid probe = %d: %s", w.Code, w.Body)
	}
	if len(f.probes) != 1 || f.probes[0].UpdatedBy != "user-admin" {
		t.Errorf("probes = %+v, want one row updated_by user-admin", f.probes)
	}
}

func TestConfigProbePutImmutableIdentity(t *testing.T) {
	f := newFakeDB()
	id := uuid.New()
	f.probes = []store.ProbeConfigInfo{{
		ID: id, Site: "nyc", Target: "pg", ProbeType: int16(pb.ProbeType_PROBE_TYPE_TCP),
		Interval: 10 * time.Second, Timeout: 5 * time.Second, Enabled: true,
	}}
	h := newTestAPI(t, f)
	cookie, csrf := configLogin(t, h, f, "admin")

	// Changing identity fields is rejected, each named.
	body := `{"site":"lon","target":"pg","type":"icmp","interval_ms":10000,"timeout_ms":5000}`
	w := doConfig(t, h, "PUT", "/api/v1/config/probes/"+id.String(), body, cookie, csrf)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("identity change = %d, want 400: %s", w.Code, w.Body)
	}
	msg := errBody(t, w)
	for _, want := range []string{"type cannot be changed", "site cannot be changed"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}

	// A cadence edit (identity omitted) succeeds and keeps the ID.
	body = `{"interval_ms":20000,"timeout_ms":5000,"params":{}}`
	w = doConfig(t, h, "PUT", "/api/v1/config/probes/"+id.String(), body, cookie, csrf)
	if w.Code != http.StatusOK {
		t.Fatalf("cadence edit = %d: %s", w.Code, w.Body)
	}
	var res struct {
		ID         string `json:"id"`
		IntervalMS int64  `json:"interval_ms"`
		Enabled    bool   `json:"enabled"`
	}
	json.Unmarshal(w.Body.Bytes(), &res)
	if res.ID != id.String() || res.IntervalMS != 20000 || !res.Enabled {
		t.Errorf("response = %+v", res)
	}

	// enabled:false round-trips (enable/disable is a full-object PUT).
	body = `{"interval_ms":20000,"timeout_ms":5000,"params":{},"enabled":false}`
	w = doConfig(t, h, "PUT", "/api/v1/config/probes/"+id.String(), body, cookie, csrf)
	if w.Code != http.StatusOK {
		t.Fatalf("disable = %d: %s", w.Code, w.Body)
	}
	json.Unmarshal(w.Body.Bytes(), &res)
	if res.Enabled {
		t.Error("enabled should be false after disable PUT")
	}

	// Bad UUID and unknown ID.
	if w := doConfig(t, h, "PUT", "/api/v1/config/probes/not-a-uuid", body, cookie, csrf); w.Code != http.StatusBadRequest {
		t.Errorf("bad uuid = %d, want 400", w.Code)
	}
	if w := doConfig(t, h, "PUT", "/api/v1/config/probes/"+uuid.New().String(), body, cookie, csrf); w.Code != http.StatusNotFound {
		t.Errorf("unknown id = %d, want 404", w.Code)
	}
}

func TestConfigMeshDeleteReportsCascade(t *testing.T) {
	f := newFakeDB()
	f.meshes = []store.MeshGroupInfo{{ID: uuid.New(), Name: "core", Sites: []string{"nyc", "lon"}, ProbeCount: 2}}
	h := newTestAPI(t, f)
	cookie, csrf := configLogin(t, h, f, "admin")

	w := doConfig(t, h, "DELETE", "/api/v1/config/meshes/core", "", cookie, csrf)
	if w.Code != http.StatusOK {
		t.Fatalf("mesh delete = %d: %s", w.Code, w.Body)
	}
	var res struct {
		ProbesDeleted int64 `json:"probes_deleted"`
	}
	json.Unmarshal(w.Body.Bytes(), &res)
	if res.ProbesDeleted != 2 {
		t.Errorf("probes_deleted = %d, want 2", res.ProbesDeleted)
	}
}

func TestConfigProbeTypesRegistry(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, _ := configLogin(t, h, f, "viewer")

	w := doConfig(t, h, "GET", "/api/v1/config/probe-types", "", cookie, "")
	if w.Code != http.StatusOK {
		t.Fatalf("probe-types = %d: %s", w.Code, w.Body)
	}
	var res struct {
		Types []struct {
			Type       string `json:"type"`
			DirectOnly bool   `json:"direct_only"`
			Params     []struct {
				Key          string `json:"key"`
				Kind         string `json:"kind"`
				Min          int    `json:"min"`
				Max          int    `json:"max"`
				RequiredMesh bool   `json:"required_mesh"`
				MeshOnly     bool   `json:"mesh_only"`
			} `json:"params"`
		} `json:"types"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("bad body: %v", err)
	}
	byType := map[string][]string{}
	directOnly := map[string]bool{}
	var tcpHasMeshOnlyPort, mtuHasIntBounds bool
	for _, tt := range res.Types {
		directOnly[tt.Type] = tt.DirectOnly
		for _, p := range tt.Params {
			byType[tt.Type] = append(byType[tt.Type], p.Key)
			if tt.Type == "tcp" && p.Key == "port" && p.MeshOnly && p.RequiredMesh {
				tcpHasMeshOnlyPort = true
			}
			if tt.Type == "path_mtu" && p.Key == "mtu.min" && p.Kind == "int" && p.Min == 68 && p.Max == 9216 {
				mtuHasIntBounds = true
			}
		}
	}
	if len(res.Types) != 8 {
		t.Errorf("types = %d, want 8", len(res.Types))
	}
	if !tcpHasMeshOnlyPort {
		t.Error("tcp must declare mesh-only required port")
	}
	if len(byType["icmp"]) != 0 {
		t.Errorf("icmp params = %v, want none", byType["icmp"])
	}
	if want := []string{"dns.qname", "dns.qtype", "dns.expect_rcode", "dns.resolver"}; !slicesEqual(byType["dns"], want) {
		t.Errorf("dns params = %v, want %v", byType["dns"], want)
	}
	// The SPA hides direct-only types from mesh mode off this flag.
	if !directOnly["http"] || !directOnly["ntp"] {
		t.Errorf("direct_only flags = %v, want http and ntp true", directOnly)
	}
	if directOnly["icmp"] || directOnly["tcp"] {
		t.Errorf("direct_only flags = %v, want icmp and tcp false", directOnly)
	}
	if len(byType["ntp"]) != 0 {
		t.Errorf("ntp params = %v, want none", byType["ntp"])
	}
	if want := []string{"mtu.min", "mtu.max", "mtu.family"}; !slicesEqual(byType["path_mtu"], want) {
		t.Errorf("path_mtu params = %v, want %v", byType["path_mtu"], want)
	}
	// The SPA renders int params with client-side bounds off min/max.
	if !mtuHasIntBounds {
		t.Error("path_mtu mtu.min must declare kind int with min/max bounds")
	}
	if directOnly["path_mtu"] {
		t.Error("path_mtu must support mesh templates")
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestConfigProbeMeshDNSWarning pins that the mesh-dns advisory reaches the
// API caller on the SUCCESS path. It must not block the create — the UI
// shows it so an operator learns what the probe will actually query before
// waiting for a dashboard that never fills in.
func TestConfigProbeMeshDNSWarning(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, csrf := configLogin(t, h, f, "admin")

	w := doConfig(t, h, "POST", "/api/v1/config/probes",
		`{"mesh":"edge","type":"dns","interval_ms":60000,"timeout_ms":5000,"params":{"dns.qname":"example.internal"}}`,
		cookie, csrf)
	if w.Code != http.StatusOK {
		t.Fatalf("mesh dns create = %d, want 200 (advisory, not a rejection): %s", w.Code, w.Body)
	}
	var got struct {
		ID       string   `json:"id"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID == "" {
		t.Error("a warned create must still return the new probe id")
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "dns.resolver") {
		t.Fatalf("warnings = %v, want one naming dns.resolver", got.Warnings)
	}

	// An explicit resolver is a deliberate choice: no warning, same 200.
	w = doConfig(t, h, "POST", "/api/v1/config/probes",
		`{"mesh":"edge","type":"dns","interval_ms":60000,"timeout_ms":5000,`+
			`"params":{"dns.qname":"example.internal","dns.resolver":"10.0.0.53:53"}}`,
		cookie, csrf)
	if w.Code != http.StatusOK {
		t.Fatalf("resolver-scoped mesh dns = %d, want 200: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "warnings") {
		t.Errorf("explicit dns.resolver must not warn: %s", w.Body)
	}

	// Probes with nothing to flag carry no warnings key at all.
	w = doConfig(t, h, "POST", "/api/v1/config/probes", validDirectProbe, cookie, csrf)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "warnings") {
		t.Errorf("unremarkable probe = %d with body %s, want 200 and no warnings key", w.Code, w.Body)
	}
}

// TestConfigProbeNTPCadenceWarning pins that the ntp rate-limit advisory
// reaches the API caller on the success path — a create, never a rejection,
// because operators probing their own time servers may poll faster.
func TestConfigProbeNTPCadenceWarning(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, csrf := configLogin(t, h, f, "admin")

	w := doConfig(t, h, "POST", "/api/v1/config/probes",
		`{"site":"nyc","target":"ntp-nyc","type":"ntp","interval_ms":30000,"timeout_ms":5000}`, cookie, csrf)
	if w.Code != http.StatusOK {
		t.Fatalf("fast ntp create = %d, want 200 (advisory, not a rejection): %s", w.Code, w.Body)
	}
	var got struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "rate limiting") {
		t.Fatalf("warnings = %v, want one naming rate limiting", got.Warnings)
	}

	// A polite cadence is unremarkable: no warnings key at all.
	w = doConfig(t, h, "POST", "/api/v1/config/probes",
		`{"site":"nyc","target":"ntp-nyc","type":"ntp","interval_ms":60000,"timeout_ms":5000}`, cookie, csrf)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "warnings") {
		t.Errorf("polite ntp = %d with body %s, want 200 and no warnings key", w.Code, w.Body)
	}
}

// TestConfigProbePutMeshDNSWarning pins that the advisory survives the EDIT
// path. Params are editable in place, so clearing dns.resolver on an existing
// mesh dns probe reaches the same broken configuration a create would — a
// warning only on POST would be trivially bypassed by the normal edit flow.
func TestConfigProbePutMeshDNSWarning(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, csrf := configLogin(t, h, f, "admin")

	// Create with an explicit resolver: no warning.
	w := doConfig(t, h, "POST", "/api/v1/config/probes",
		`{"mesh":"edge","type":"dns","interval_ms":60000,"timeout_ms":5000,`+
			`"params":{"dns.qname":"example.internal","dns.resolver":"10.0.0.53:53"}}`,
		cookie, csrf)
	if w.Code != http.StatusOK {
		t.Fatalf("create = %d: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "warnings") {
		t.Fatalf("resolver-scoped create must not warn: %s", w.Body)
	}
	id := f.probes[0].ID

	// Edit it to drop the resolver: same configuration a warned create makes.
	w = doConfig(t, h, "PUT", "/api/v1/config/probes/"+id.String(),
		`{"interval_ms":60000,"timeout_ms":5000,"params":{"dns.qname":"example.internal"}}`,
		cookie, csrf)
	if w.Code != http.StatusOK {
		t.Fatalf("edit = %d, want 200: %s", w.Code, w.Body)
	}
	var got struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "dns.resolver") {
		t.Fatalf("edit warnings = %v, want one naming dns.resolver", got.Warnings)
	}

	// Restoring the resolver clears the advisory.
	w = doConfig(t, h, "PUT", "/api/v1/config/probes/"+id.String(),
		`{"interval_ms":60000,"timeout_ms":5000,`+
			`"params":{"dns.qname":"example.internal","dns.resolver":"10.0.0.53:53"}}`,
		cookie, csrf)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "warnings") {
		t.Errorf("restored resolver = %d with %s, want 200 and no warnings", w.Code, w.Body)
	}
}

// TestConfigProbeDisabledSuppressesWarning pins that stopping a probe does
// not lecture the operator about what it would have queried. A disabled
// probe measures nothing, so the advisory would describe a run that will
// never happen.
func TestConfigProbeDisabledSuppressesWarning(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, csrf := configLogin(t, h, f, "admin")

	w := doConfig(t, h, "POST", "/api/v1/config/probes",
		`{"mesh":"edge","type":"dns","interval_ms":60000,"timeout_ms":5000,"params":{"dns.qname":"a.internal"}}`,
		cookie, csrf)
	if w.Code != http.StatusOK {
		t.Fatalf("create = %d: %s", w.Code, w.Body)
	}
	id := f.probes[0].ID

	body := `{"interval_ms":60000,"timeout_ms":5000,"params":{"dns.qname":"a.internal"},"enabled":%s}`
	w = doConfig(t, h, "PUT", "/api/v1/config/probes/"+id.String(), fmt.Sprintf(body, "false"), cookie, csrf)
	if w.Code != http.StatusOK {
		t.Fatalf("disable = %d: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "warnings") {
		t.Errorf("disabling must not warn about what the probe would query: %s", w.Body)
	}

	// Re-enabling the same configuration warns again: that is the moment an
	// upgraded installation first hears about it.
	w = doConfig(t, h, "PUT", "/api/v1/config/probes/"+id.String(), fmt.Sprintf(body, "true"), cookie, csrf)
	if w.Code != http.StatusOK {
		t.Fatalf("enable = %d: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "dns.resolver") {
		t.Errorf("re-enabling a warned config must warn: %s", w.Body)
	}
}

// TestConfigProbePostEnabled pins that create honors the enabled field: a
// client that sends "enabled": false gets a disabled probe, not a silently
// enabled one (issue #32).
func TestConfigProbePostEnabled(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, csrf := configLogin(t, h, f, "admin")

	const validMeshProbe = `{"mesh":"edge","type":"tcp","interval_ms":10000,"timeout_ms":5000,"params":{"port":"5432"}}`
	cases := []struct {
		name, body  string
		wantEnabled bool
	}{
		{"direct omitted defaults enabled", validDirectProbe, true},
		{"direct explicit true", strings.Replace(validDirectProbe, `"params":{}`, `"params":{},"enabled":true`, 1), true},
		{"direct explicit false", strings.Replace(validDirectProbe, `"params":{}`, `"params":{},"enabled":false`, 1), false},
		{"mesh omitted defaults enabled", validMeshProbe, true},
		{"mesh explicit false", strings.Replace(validMeshProbe, `}}`, `},"enabled":false}`, 1), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := doConfig(t, h, "POST", "/api/v1/config/probes", c.body, cookie, csrf)
			if w.Code != http.StatusOK {
				t.Fatalf("create = %d: %s", w.Code, w.Body)
			}
			got := f.probes[len(f.probes)-1]
			if got.Enabled != c.wantEnabled {
				t.Errorf("enabled = %v, want %v", got.Enabled, c.wantEnabled)
			}
		})
	}
}

// TestConfigProbePostDisabledSuppressesWarning is the create-side twin of
// TestConfigProbeDisabledSuppressesWarning: a probe born disabled measures
// nothing, so the advisory would describe a run that will never happen. It
// arrives when the probe is enabled instead.
func TestConfigProbePostDisabledSuppressesWarning(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, csrf := configLogin(t, h, f, "admin")

	body := `{"mesh":"edge","type":"dns","interval_ms":60000,"timeout_ms":5000,"params":{"dns.qname":"a.internal"},"enabled":%s}`
	w := doConfig(t, h, "POST", "/api/v1/config/probes", fmt.Sprintf(body, "false"), cookie, csrf)
	if w.Code != http.StatusOK {
		t.Fatalf("disabled create = %d: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "warnings") {
		t.Errorf("disabled create must not warn about what the probe would query: %s", w.Body)
	}

	// The same configuration created enabled warns as before.
	w = doConfig(t, h, "POST", "/api/v1/config/probes", fmt.Sprintf(body, "true"), cookie, csrf)
	if w.Code != http.StatusOK {
		t.Fatalf("enabled create = %d: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "dns.resolver") {
		t.Errorf("enabled create of a warned config must warn: %s", w.Body)
	}
}

func TestConfigMeshAndProbeNetwork(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, csrf := configLogin(t, h, f, "admin")
	f.networks = append(f.networks, store.NetworkAdminInfo{ID: uuid.New(), Name: "mgmt"})

	// A mesh binds its network at creation; re-upserting with a different
	// one is refused (immutable), with no opinion it keeps the binding, and
	// a typo'd network is a 404.
	if w := doConfig(t, h, "POST", "/api/v1/config/meshes", `{"name":"m1","network":"mgmt"}`, cookie, csrf); w.Code != http.StatusOK {
		t.Fatalf("POST mesh on mgmt = %d: %s", w.Code, w.Body)
	}
	if w := doConfig(t, h, "POST", "/api/v1/config/meshes", `{"name":"m1","network":"default"}`, cookie, csrf); w.Code != http.StatusConflict {
		t.Errorf("cross-network mesh re-post = %d, want 409", w.Code)
	}
	if w := doConfig(t, h, "POST", "/api/v1/config/meshes", `{"name":"m1"}`, cookie, csrf); w.Code != http.StatusOK {
		t.Errorf("no-opinion mesh re-post = %d, want 200 (keeps mgmt)", w.Code)
	}
	if w := doConfig(t, h, "POST", "/api/v1/config/meshes", `{"name":"m2","network":"typo"}`, cookie, csrf); w.Code != http.StatusNotFound {
		t.Errorf("mesh on unknown network = %d, want 404", w.Code)
	}
	var meshList struct {
		Meshes []struct {
			Name    string `json:"name"`
			Network string `json:"network"`
		} `json:"meshes"`
	}
	w := doConfig(t, h, "GET", "/api/v1/config/meshes", "", cookie, "")
	if err := json.Unmarshal(w.Body.Bytes(), &meshList); err != nil ||
		len(meshList.Meshes) != 1 || meshList.Meshes[0].Network != "mgmt" {
		t.Errorf("mesh list %q (err %v), want m1 on mgmt", w.Body, err)
	}

	// Direct probes carry their own network; mesh probes inherit and refuse
	// an explicit one; a typo'd network is a 404.
	w = doConfig(t, h, "POST", "/api/v1/config/probes",
		`{"site":"site-a","target":"svc","network":"mgmt","type":"icmp","interval_ms":30000,"timeout_ms":5000}`, cookie, csrf)
	if w.Code != http.StatusOK {
		t.Fatalf("direct probe on mgmt = %d: %s", w.Code, w.Body)
	}
	if len(f.probes) != 1 || f.probes[0].Network != "mgmt" {
		t.Errorf("stored probe = %+v, want network mgmt", f.probes)
	}
	w = doConfig(t, h, "POST", "/api/v1/config/probes",
		`{"mesh":"m1","network":"mgmt","type":"icmp","interval_ms":30000,"timeout_ms":5000}`, cookie, csrf)
	if w.Code != http.StatusBadRequest || !strings.Contains(errBody(t, w), "inherit") {
		t.Errorf("mesh probe with network = %d %q, want 400 naming inheritance", w.Code, w.Body)
	}
	w = doConfig(t, h, "POST", "/api/v1/config/probes",
		`{"site":"site-a","target":"svc","network":"typo","type":"icmp","interval_ms":30000,"timeout_ms":5000}`, cookie, csrf)
	if w.Code != http.StatusNotFound {
		t.Errorf("direct probe on unknown network = %d, want 404", w.Code)
	}

	// The list surfaces the network, and PUT treats it as immutable
	// identity.
	var probeList struct {
		Probes []struct {
			ID      string `json:"id"`
			Network string `json:"network"`
		} `json:"probes"`
	}
	w = doConfig(t, h, "GET", "/api/v1/config/probes", "", cookie, "")
	if err := json.Unmarshal(w.Body.Bytes(), &probeList); err != nil ||
		len(probeList.Probes) != 1 || probeList.Probes[0].Network != "mgmt" {
		t.Fatalf("probe list %q (err %v), want one probe on mgmt", w.Body, err)
	}
	w = doConfig(t, h, "PUT", "/api/v1/config/probes/"+probeList.Probes[0].ID,
		`{"network":"default","interval_ms":30000,"timeout_ms":5000}`, cookie, csrf)
	if w.Code != http.StatusBadRequest || !strings.Contains(errBody(t, w), "network cannot be changed in place") {
		t.Errorf("PUT network change = %d %q, want 400", w.Code, w.Body)
	}
}
