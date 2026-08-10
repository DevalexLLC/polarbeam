package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/devalexllc/polarbeam/internal/server/store"
)

// loginRole logs in a fresh user with the given role and returns the session
// cookie + CSRF token (loginAndCookie is viewer-only).
func loginRole(t *testing.T, h http.Handler, f *fakeDB, username, role string) (*http.Cookie, string) {
	t.Helper()
	f.addUser(username, "hunter22222", role, false)
	w := doLogin(t, h, username, "hunter22222")
	if w.Code != http.StatusOK {
		t.Fatalf("login failed: %d %s", w.Code, w.Body)
	}
	var res struct {
		CSRFToken string `json:"csrf_token"`
	}
	json.Unmarshal(w.Body.Bytes(), &res)
	return w.Result().Cookies()[0], res.CSRFToken
}

func putSettings(h http.Handler, cookie *http.Cookie, csrf string, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("PUT", "/api/v1/settings", strings.NewReader(body))
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

func TestSettingsRequireAuth(t *testing.T) {
	h := newTestAPI(t, newFakeDB())
	for _, m := range []string{"GET", "PUT"} {
		req := httptest.NewRequest(m, "/api/v1/settings", strings.NewReader("{}"))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s without session = %d, want 401", m, w.Code)
		}
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, csrf := loginRole(t, h, f, "root", "admin")

	// GET before any write serves the defaults.
	req := httptest.NewRequest("GET", "/api/v1/settings", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET = %d: %s", w.Code, w.Body)
	}
	var got settingsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad GET body: %v", err)
	}
	if got.Thresholds.LatencyWarnUS != 100000 || got.Thresholds.LossCritPct != 5 {
		t.Errorf("defaults = %+v", got.Thresholds)
	}

	w = putSettings(h, cookie, csrf,
		`{"latency_warn_us":50000,"latency_crit_us":200000,"loss_warn_pct":0.5,"loss_crit_pct":3}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", w.Code, w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad PUT body: %v", err)
	}
	if got.Thresholds.LatencyWarnUS != 50000 || got.Thresholds.LossCritPct != 3 {
		t.Errorf("PUT echo = %+v", got.Thresholds)
	}
	if got.UpdatedBy != "root" {
		t.Errorf("updated_by = %q, want the session username", got.UpdatedBy)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("updated_at not stamped")
	}

	// GET reflects the write.
	req = httptest.NewRequest("GET", "/api/v1/settings", nil)
	req.AddCookie(cookie)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	json.Unmarshal(w.Body.Bytes(), &got)
	if got.Thresholds.LatencyCritUS != 200000 {
		t.Errorf("GET after PUT = %+v", got.Thresholds)
	}
}

func TestSettingsPutViewerForbidden(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, csrf := loginAndCookie(t, h, f) // viewer

	w := putSettings(h, cookie, csrf,
		`{"latency_warn_us":50000,"latency_crit_us":200000,"loss_warn_pct":0.5,"loss_crit_pct":3}`)
	if w.Code != http.StatusForbidden {
		t.Errorf("viewer PUT = %d, want 403: %s", w.Code, w.Body)
	}
	if f.settings != nil {
		t.Error("viewer PUT must not write settings")
	}
	// The viewer can still read.
	req := httptest.NewRequest("GET", "/api/v1/settings", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("viewer GET = %d, want 200", rec.Code)
	}
}

func TestSettingsPutCSRF(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, _ := loginRole(t, h, f, "root", "admin")

	for name, token := range map[string]string{"missing": "", "wrong": "not-the-token"} {
		w := putSettings(h, cookie, token,
			`{"latency_warn_us":50000,"latency_crit_us":200000,"loss_warn_pct":0.5,"loss_crit_pct":3}`)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s CSRF token: PUT = %d, want 403", name, w.Code)
		}
	}
}

func TestSettingsPutValidation(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, csrf := loginRole(t, h, f, "root", "admin")

	cases := map[string]string{
		"warn not below crit": `{"latency_warn_us":200000,"latency_crit_us":200000,"loss_warn_pct":1,"loss_crit_pct":5}`,
		"zero latency warn":   `{"latency_warn_us":0,"latency_crit_us":200000,"loss_warn_pct":1,"loss_crit_pct":5}`,
		"latency over 60s":    `{"latency_warn_us":100,"latency_crit_us":60000001,"loss_warn_pct":1,"loss_crit_pct":5}`,
		"negative loss warn":  `{"latency_warn_us":100,"latency_crit_us":200,"loss_warn_pct":-1,"loss_crit_pct":5}`,
		"loss over 100":       `{"latency_warn_us":100,"latency_crit_us":200,"loss_warn_pct":1,"loss_crit_pct":101}`,
		"loss warn not below": `{"latency_warn_us":100,"latency_crit_us":200,"loss_warn_pct":5,"loss_crit_pct":5}`,
		"unknown key":         `{"latency_warn_us":100,"latency_crit_us":200,"loss_warn_pct":1,"loss_crit_pct":5,"surprise":1}`,
		"not json":            `latency=fast please`,
		"trailing data":       `{"latency_warn_us":100,"latency_crit_us":200,"loss_warn_pct":1,"loss_crit_pct":5}{"again":1}`,
	}
	for name, body := range cases {
		w := putSettings(h, cookie, csrf, body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: PUT = %d, want 400: %s", name, w.Code, w.Body)
		}
		if f.settings != nil {
			t.Fatalf("%s: invalid PUT must not write settings", name)
		}
	}
}

func TestValidateThresholdsNamesEveryProblem(t *testing.T) {
	err := validateThresholds(thresholdsJSON{
		LatencyWarnUS: 0, LatencyCritUS: 0, LossWarnPct: 50, LossCritPct: 5,
	})
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"latency_warn_us", "latency_crit_us", "loss_crit_pct"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}

func TestSitesCarryCoordinates(t *testing.T) {
	f := newFakeDB()
	lat, lon := 51.5074, -0.1278
	f.sites = []store.SiteInfo{
		{Name: "lon", DisplayName: "London", Latitude: &lat, Longitude: &lon},
		{Name: "nyc"},
	}
	h := newTestAPI(t, f)
	cookie, _ := loginAndCookie(t, h, f)

	for _, path := range []string{"/api/v1/sites", "/api/v1/matrix"} {
		req := httptest.NewRequest("GET", path, nil)
		req.AddCookie(cookie)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s = %d: %s", path, w.Code, w.Body)
		}
		var res struct {
			Sites []map[string]json.RawMessage `json:"sites"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
			t.Fatalf("%s: bad body: %v", path, err)
		}
		if len(res.Sites) != 2 {
			t.Fatalf("%s: sites = %d, want 2", path, len(res.Sites))
		}
		if !bytes.Contains(res.Sites[0]["latitude"], []byte("51.5074")) {
			t.Errorf("%s: placed site latitude = %s", path, res.Sites[0]["latitude"])
		}
		// Unset coordinates must be an explicit null, not absent and not 0.
		if string(res.Sites[1]["latitude"]) != "null" || string(res.Sites[1]["longitude"]) != "null" {
			t.Errorf("%s: unplaced site coords = %s/%s, want null/null",
				path, res.Sites[1]["latitude"], res.Sites[1]["longitude"])
		}
	}
}
