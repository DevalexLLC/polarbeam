package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/devalexllc/polarbeam/internal/server/store"
)

// ---- fakeDB path-threshold methods ----

var _ thresholdStore = (*fakeDB)(nil)

// fakeThresholdState backs the thresholdStore fake methods (GetSettings /
// UpdateSettings live in httpapi_test.go, the override layers here).
type fakeThresholdState struct {
	settings *store.ThresholdSettings
	// key: lexically sorted site names joined with \x00 (the fake
	// canonicalizes by name where the real store uses uuid order)
	pathThresholds    map[string]*store.PathThresholdOverride
	networkThresholds map[string]*store.NetworkThreshold
}

// fakePairKey canonicalizes by name where the real store canonicalizes by
// uuid order — equivalent for the fake's purposes (both orders of a PUT
// must land on one row).
// fakePairKey mirrors the store's three-part key: the unordered pair plus
// the plane, where "" is the all-planes row.
func fakePairKey(a, b, network string) string {
	if a > b {
		a, b = b, a
	}
	return a + "\x00" + b + "\x00" + network
}

// networkName resolves a plane id back to its name the way the store's
// joins do; "" for the all-planes row.
func (f *fakeDB) networkName(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	for _, n := range f.networks {
		if n.ID == *id {
			return n.Name
		}
	}
	return ""
}

func (f *fakeDB) ListPathThresholds(_ context.Context, networks []uuid.UUID) ([]store.PathThresholdOverride, error) {
	f.recordScope("ListPathThresholds", networks)
	out := make([]store.PathThresholdOverride, 0, len(f.pathThresholds))
	for _, k := range slices.Sorted(maps.Keys(f.pathThresholds)) {
		out = append(out, *f.pathThresholds[k])
	}
	return out, nil
}

func (f *fakeDB) UpsertPathThreshold(ctx context.Context, siteA, siteB string, networkID *uuid.UUID, o store.PathThresholdOverride) (*store.PathThresholdOverride, error) {
	if _, err := f.SiteIDByName(ctx, siteA); err != nil {
		return nil, err
	}
	if _, err := f.SiteIDByName(ctx, siteB); err != nil {
		return nil, err
	}
	if siteA > siteB {
		siteA, siteB = siteB, siteA
	}
	o.A, o.B, o.Network = siteA, siteB, f.networkName(networkID)
	o.UpdatedAt = time.Now()
	if f.pathThresholds == nil {
		f.pathThresholds = map[string]*store.PathThresholdOverride{}
	}
	f.pathThresholds[fakePairKey(siteA, siteB, o.Network)] = &o
	return &o, nil
}

func (f *fakeDB) DeletePathThreshold(ctx context.Context, siteA, siteB string, networkID *uuid.UUID) error {
	if _, err := f.SiteIDByName(ctx, siteA); err != nil {
		return err
	}
	if _, err := f.SiteIDByName(ctx, siteB); err != nil {
		return err
	}
	key := fakePairKey(siteA, siteB, f.networkName(networkID))
	if _, ok := f.pathThresholds[key]; !ok {
		return fmt.Errorf("no threshold override for %s and %s%w", siteA, siteB, store.ErrNotFound)
	}
	delete(f.pathThresholds, key)
	return nil
}

func (f *fakeDB) ListNetworkThresholds(_ context.Context, networks []uuid.UUID) ([]store.NetworkThreshold, error) {
	f.recordScope("ListNetworkThresholds", networks)
	out := make([]store.NetworkThreshold, 0, len(f.networkThresholds))
	for _, k := range slices.Sorted(maps.Keys(f.networkThresholds)) {
		out = append(out, *f.networkThresholds[k])
	}
	return out, nil
}

func (f *fakeDB) UpsertNetworkThreshold(ctx context.Context, network string, t store.NetworkThreshold, scope []uuid.UUID) (*store.NetworkThreshold, error) {
	id, err := f.NetworkIDByName(ctx, network)
	if err != nil {
		return nil, err
	}
	if scope != nil && !slices.Contains(scope, id) {
		return nil, fmt.Errorf("network %q does not exist%w", network, store.ErrNotFound)
	}
	t.Network, t.UpdatedAt = network, time.Now()
	if f.networkThresholds == nil {
		f.networkThresholds = map[string]*store.NetworkThreshold{}
	}
	f.networkThresholds[network] = &t
	return &t, nil
}

func (f *fakeDB) DeleteNetworkThreshold(ctx context.Context, network string, scope []uuid.UUID) error {
	id, err := f.NetworkIDByName(ctx, network)
	if err != nil {
		return err
	}
	if scope != nil && !slices.Contains(scope, id) {
		return fmt.Errorf("network %q does not exist%w", network, store.ErrNotFound)
	}
	if _, ok := f.networkThresholds[network]; !ok {
		return fmt.Errorf("no threshold overlay for network %q%w", network, store.ErrNotFound)
	}
	delete(f.networkThresholds, network)
	return nil
}

// ---- helpers ----

func seedPairSites(f *fakeDB, names ...string) {
	for _, n := range names {
		f.siteConfigs = append(f.siteConfigs, store.SiteAdminInfo{
			SiteInfo: store.SiteInfo{ID: uuid.New(), Name: n},
		})
	}
}

func doPathThreshold(h http.Handler, method string, cookie *http.Cookie, csrf, a, b, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/api/v1/settings/path-thresholds/"+a+"/"+b, strings.NewReader(body))
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

func getSettingsResponse(t *testing.T, h http.Handler, cookie *http.Cookie) settingsResponse {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/v1/settings", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /settings = %d: %s", w.Code, w.Body)
	}
	var got settingsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad settings body: %v", err)
	}
	return got
}

// ---- tests ----

func TestPathThresholdRoundTrip(t *testing.T) {
	f := newFakeDB()
	seedPairSites(f, "syd", "co")
	h := newTestAPI(t, f)
	cookie, csrf := loginRole(t, h, f, "root", "admin")

	// Partial override: latency only, loss inherits.
	w := doPathThreshold(h, "PUT", cookie, csrf, "syd", "co",
		`{"latency_warn_us":180000,"latency_crit_us":null,"loss_warn_pct":null,"loss_crit_pct":null}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", w.Code, w.Body)
	}
	var res pathThresholdWriteResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("bad PUT body: %v", err)
	}
	// Names come back lexically sorted regardless of URL order.
	if res.A != "co" || res.B != "syd" {
		t.Errorf("pair = %s/%s, want co/syd", res.A, res.B)
	}
	if res.LatencyWarnUS == nil || *res.LatencyWarnUS != 180000 || res.LatencyCritUS != nil {
		t.Errorf("override fields = %+v", res.pathThresholdFields)
	}
	// Effective tuple merges the override over the (default) global row.
	if res.Effective.LatencyWarnUS != 180000 || res.Effective.LatencyCritUS != 250000 ||
		res.Effective.LossWarnPct != 1 || res.Effective.LossCritPct != 5 {
		t.Errorf("effective = %+v", res.Effective)
	}
	if res.UpdatedBy != "root" || res.UpdatedAt.IsZero() {
		t.Errorf("audit fields = %q %v", res.UpdatedBy, res.UpdatedAt)
	}

	// GET /settings carries the override with explicit nulls.
	got := getSettingsResponse(t, h, cookie)
	if len(got.Overrides) != 1 {
		t.Fatalf("overrides = %d, want 1", len(got.Overrides))
	}
	o := got.Overrides[0]
	if o.A != "co" || o.B != "syd" || o.LatencyWarnUS == nil || *o.LatencyWarnUS != 180000 || o.LossCritPct != nil {
		t.Errorf("listed override = %+v", o)
	}

	// The reversed URL order updates the SAME row.
	w = doPathThreshold(h, "PUT", cookie, csrf, "co", "syd",
		`{"latency_warn_us":190000,"latency_crit_us":null,"loss_warn_pct":null,"loss_crit_pct":null}`)
	if w.Code != http.StatusOK {
		t.Fatalf("reversed PUT = %d: %s", w.Code, w.Body)
	}
	if len(f.pathThresholds) != 1 {
		t.Fatalf("reversed PUT created a second row: %d", len(f.pathThresholds))
	}
	got = getSettingsResponse(t, h, cookie)
	if len(got.Overrides) != 1 || *got.Overrides[0].LatencyWarnUS != 190000 {
		t.Errorf("after reversed PUT: %+v", got.Overrides)
	}
}

func TestPathThresholdViewerForbidden(t *testing.T) {
	f := newFakeDB()
	seedPairSites(f, "syd", "co")
	h := newTestAPI(t, f)
	cookie, csrf := loginAndCookie(t, h, f) // viewer

	w := doPathThreshold(h, "PUT", cookie, csrf, "syd", "co", `{"latency_warn_us":180000}`)
	if w.Code != http.StatusForbidden {
		t.Errorf("viewer PUT = %d, want 403: %s", w.Code, w.Body)
	}
	w = doPathThreshold(h, "DELETE", cookie, csrf, "syd", "co", "")
	if w.Code != http.StatusForbidden {
		t.Errorf("viewer DELETE = %d, want 403: %s", w.Code, w.Body)
	}
	if len(f.pathThresholds) != 0 {
		t.Error("viewer write must not touch the store")
	}
	// The viewer still sees overrides via GET /settings.
	if got := getSettingsResponse(t, h, cookie); got.Overrides == nil {
		t.Error("viewer GET missing overrides array")
	}
}

func TestPathThresholdCSRF(t *testing.T) {
	f := newFakeDB()
	seedPairSites(f, "syd", "co")
	h := newTestAPI(t, f)
	cookie, _ := loginRole(t, h, f, "root", "admin")

	for name, token := range map[string]string{"missing": "", "wrong": "not-the-token"} {
		w := doPathThreshold(h, "PUT", cookie, token, "syd", "co", `{"latency_warn_us":180000}`)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s CSRF token: PUT = %d, want 403", name, w.Code)
		}
	}
	if len(f.pathThresholds) != 0 {
		t.Error("CSRF-failed PUT must not write")
	}
}

func TestPathThresholdNoSession(t *testing.T) {
	h := newTestAPI(t, newFakeDB())
	for _, m := range []string{"PUT", "DELETE"} {
		w := doPathThreshold(h, m, nil, "", "syd", "co", "{}")
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s without session = %d, want 401", m, w.Code)
		}
	}
}

func TestPathThresholdValidation(t *testing.T) {
	f := newFakeDB()
	seedPairSites(f, "syd", "co")
	h := newTestAPI(t, f)
	cookie, csrf := loginRole(t, h, f, "root", "admin")

	cases := map[string]string{
		"all null":              `{"latency_warn_us":null,"latency_crit_us":null,"loss_warn_pct":null,"loss_crit_pct":null}`,
		"empty object":          `{}`,
		"zero latency warn":     `{"latency_warn_us":0}`,
		"negative latency crit": `{"latency_crit_us":-1}`,
		"latency over 60s":      `{"latency_crit_us":60000001}`,
		"negative loss warn":    `{"loss_warn_pct":-1}`,
		"loss over 100":         `{"loss_crit_pct":101}`,
		// Effective checks: the set field collides with the inherited
		// global value (defaults: crit 250000 µs, warn 1%).
		"warn above inherited crit":      `{"latency_warn_us":300000}`,
		"loss crit below inherited warn": `{"loss_crit_pct":0.5}`,
		"both set inverted":              `{"latency_warn_us":200000,"latency_crit_us":100000}`,
		"unknown key":                    `{"latency_warn_us":180000,"surprise":1}`,
		"not json":                       `latency=fast please`,
		"trailing data":                  `{"latency_warn_us":180000}{"again":1}`,
	}
	for name, body := range cases {
		w := doPathThreshold(h, "PUT", cookie, csrf, "syd", "co", body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: PUT = %d, want 400: %s", name, w.Code, w.Body)
		}
		if len(f.pathThresholds) != 0 {
			t.Fatalf("%s: invalid PUT must not write", name)
		}
	}

	// Same site on both sides of the URL.
	w := doPathThreshold(h, "PUT", cookie, csrf, "syd", "syd", `{"latency_warn_us":180000}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("self pair PUT = %d, want 400: %s", w.Code, w.Body)
	}
}

func TestPathThresholdUnknownSite(t *testing.T) {
	f := newFakeDB()
	seedPairSites(f, "syd")
	h := newTestAPI(t, f)
	cookie, csrf := loginRole(t, h, f, "root", "admin")

	w := doPathThreshold(h, "PUT", cookie, csrf, "syd", "nowhere", `{"latency_warn_us":180000}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown site PUT = %d, want 404: %s", w.Code, w.Body)
	}
	w = doPathThreshold(h, "DELETE", cookie, csrf, "syd", "nowhere", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown site DELETE = %d, want 404: %s", w.Code, w.Body)
	}
}

func TestPathThresholdDelete(t *testing.T) {
	f := newFakeDB()
	seedPairSites(f, "syd", "co")
	h := newTestAPI(t, f)
	cookie, csrf := loginRole(t, h, f, "root", "admin")

	w := doPathThreshold(h, "PUT", cookie, csrf, "syd", "co", `{"latency_warn_us":180000}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", w.Code, w.Body)
	}
	// Reversed order deletes the same row.
	w = doPathThreshold(h, "DELETE", cookie, csrf, "co", "syd", "")
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE = %d: %s", w.Code, w.Body)
	}
	if len(f.pathThresholds) != 0 {
		t.Error("DELETE left the override behind")
	}
	w = doPathThreshold(h, "DELETE", cookie, csrf, "syd", "co", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("second DELETE = %d, want 404: %s", w.Code, w.Body)
	}
}

func TestSettingsOverridesAlwaysArray(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, csrf := loginRole(t, h, f, "root", "admin")

	req := httptest.NewRequest("GET", "/api/v1/settings", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	// The SPA maps over this without a null check: [] is load-bearing.
	if !strings.Contains(w.Body.String(), `"overrides":[]`) {
		t.Errorf("GET /settings body missing empty overrides array: %s", w.Body)
	}

	// The global PUT echo carries it too.
	w = putSettings(h, cookie, csrf,
		`{"latency_warn_us":50000,"latency_crit_us":200000,"loss_warn_pct":0.5,"loss_crit_pct":3}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"overrides":[]`) {
		t.Errorf("PUT /settings echo missing empty overrides array: %s", w.Body)
	}
}

func TestGlobalPutWarnsOnInvertedOverrides(t *testing.T) {
	f := newFakeDB()
	seedPairSites(f, "syd", "co")
	h := newTestAPI(t, f)
	cookie, csrf := loginRole(t, h, f, "root", "admin")

	// Partial override: warn 200 ms, crit inherited (default 250 ms) — valid.
	w := doPathThreshold(h, "PUT", cookie, csrf, "syd", "co", `{"latency_warn_us":200000}`)
	if w.Code != http.StatusOK {
		t.Fatalf("override PUT = %d: %s", w.Code, w.Body)
	}

	// Lowering the global crit below the override's warn must still commit,
	// but the response has to name the now-inconsistent pair.
	w = putSettings(h, cookie, csrf,
		`{"latency_warn_us":100000,"latency_crit_us":150000,"loss_warn_pct":1,"loss_crit_pct":5}`)
	if w.Code != http.StatusOK {
		t.Fatalf("global PUT = %d: %s", w.Code, w.Body)
	}
	var got settingsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad body: %v", err)
	}
	if got.Thresholds.LatencyCritUS != 150000 {
		t.Errorf("global write did not commit: %+v", got.Thresholds)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "co and syd") {
		t.Errorf("warnings = %v, want one naming the pair", got.Warnings)
	}

	// A consistent global write carries no warnings.
	w = putSettings(h, cookie, csrf,
		`{"latency_warn_us":100000,"latency_crit_us":300000,"loss_warn_pct":1,"loss_crit_pct":5}`)
	if w.Code != http.StatusOK {
		t.Fatalf("second global PUT = %d: %s", w.Code, w.Body)
	}
	got = settingsResponse{} // warnings is omitempty; a reused struct would keep the old slice
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad body: %v", err)
	}
	if len(got.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", got.Warnings)
	}
}

func TestValidateOverrideNamesEveryProblem(t *testing.T) {
	warn := int64(-5)
	crit := int64(70_000_000)
	lossW := -1.0
	lossC := 200.0
	err := validateOverride(pathThresholdFields{
		LatencyWarnUS: &warn, LatencyCritUS: &crit,
		LossWarnPct: &lossW, LossCritPct: &lossC,
	}, thresholdsJSON{LatencyWarnUS: 100000, LatencyCritUS: 250000, LossWarnPct: 1, LossCritPct: 5},
		"global settings")
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"latency_warn_us", "latency_crit_us", "loss_warn_pct", "loss_crit_pct"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}

func TestValidateOverrideNamesInheritedSide(t *testing.T) {
	warn := int64(300000) // above the inherited global crit of 250000
	err := validateOverride(pathThresholdFields{LatencyWarnUS: &warn},
		thresholdsJSON{LatencyWarnUS: 100000, LatencyCritUS: 250000, LossWarnPct: 1, LossCritPct: 5},
		"global settings")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "inherited from global settings") {
		t.Errorf("error %q does not flag the inherited side", err)
	}
}
