package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/devalexllc/polarbeam/internal/server/store"
)

func TestMonthStartUTC(t *testing.T) {
	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{"mid-month", time.Date(2026, 9, 10, 15, 4, 5, 6, time.UTC), time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
		{"first instant of a month", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
		{"last instant of a month", time.Date(2026, 8, 31, 23, 59, 59, 999_999_999, time.UTC), time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)},
		// 2026-09-01T03:00+05:00 is 2026-08-31T22:00Z: the UTC month wins
		// over the local calendar.
		{"non-UTC now still on the previous UTC month", time.Date(2026, 9, 1, 3, 0, 0, 0, time.FixedZone("x", 5*3600)), time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)},
		// 2026-08-31T22:00-05:00 is 2026-09-01T03:00Z.
		{"non-UTC now already on the next UTC month", time.Date(2026, 8, 31, 22, 0, 0, 0, time.FixedZone("y", -5*3600)), time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
		{"year boundary", time.Date(2027, 1, 1, 0, 0, 0, 1, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := monthStartUTC(c.now)
			if !got.Equal(c.want) || got.Location() != time.UTC {
				t.Errorf("monthStartUTC(%s) = %s, want %s", c.now, got, c.want)
			}
		})
	}
}

func TestSiteScores(t *testing.T) {
	f := newFakeDB()
	f.siteScores = []store.SiteScore{
		{Name: "lon", Samples: 1000, OK: 990, HealthyOK: 900},
		{Name: "nyc", Samples: 0, OK: 0, HealthyOK: 0},
	}
	h := newTestAPI(t, f)
	cookie, _ := loginAndCookie(t, h, f)

	before := monthStartUTC(time.Now())
	req := httptest.NewRequest("GET", "/api/v1/sites/scores", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	after := monthStartUTC(time.Now())
	if w.Code != http.StatusOK {
		t.Fatalf("sites/scores = %d %s", w.Code, w.Body)
	}
	var res struct {
		Month   string    `json:"month"`
		Since   time.Time `json:"since"`
		AsOf    time.Time `json:"as_of"`
		Network string    `json:"network"`
		Sites   []struct {
			Name             string `json:"name"`
			Samples          int64  `json:"samples"`
			OKSamples        int64  `json:"ok_samples"`
			HealthyOKSamples int64  `json:"healthy_ok_samples"`
		} `json:"sites"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v: %s", err, w.Body)
	}
	// The request may straddle a month boundary; either side is right, but
	// the label, the since bound, and the store's since must agree.
	if !res.Since.Equal(before) && !res.Since.Equal(after) {
		t.Errorf("since = %s, want %s or %s", res.Since, before, after)
	}
	if res.Since.UTC().Day() != 1 || res.Since.UTC().Hour() != 0 || res.Since.UTC().Minute() != 0 {
		t.Errorf("since is not a UTC month start: %s", res.Since)
	}
	if res.Month != res.Since.UTC().Format("2006-01") {
		t.Errorf("month %q does not label since %s", res.Month, res.Since)
	}
	if !f.lastScoresSince.Equal(res.Since) {
		t.Errorf("store since %s != response since %s", f.lastScoresSince, res.Since)
	}
	if res.AsOf.Before(res.Since) || res.AsOf.After(time.Now().Add(time.Minute)) {
		t.Errorf("as_of %s outside [since, now]", res.AsOf)
	}
	if f.lastScoresExclude != 6 {
		t.Errorf("exclusion = %d, want 6 (traceroute)", f.lastScoresExclude)
	}
	if res.Network != "" {
		t.Errorf("network = %q, want empty (no narrowing)", res.Network)
	}
	if got := f.scopeArgs["SiteScores"]; got != nil {
		t.Errorf("global viewer scope = %v, want nil", got)
	}
	if len(res.Sites) != 2 || res.Sites[0].Name != "lon" || res.Sites[0].Samples != 1000 ||
		res.Sites[0].OKSamples != 990 || res.Sites[0].HealthyOKSamples != 900 ||
		res.Sites[1].Name != "nyc" || res.Sites[1].Samples != 0 {
		t.Errorf("sites = %+v", res.Sites)
	}
	// Counts only: ratios are the SPA's to derive.
	if strings.Contains(w.Body.String(), "availability") || strings.Contains(w.Body.String(), "performance") {
		t.Errorf("response carries a ratio field: %s", w.Body)
	}
}

func TestSiteScoresEmptyIsAList(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, _ := loginAndCookie(t, h, f)
	req := httptest.NewRequest("GET", "/api/v1/sites/scores", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("sites/scores = %d %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"sites":[]`) {
		t.Errorf("empty result must be [] not null: %s", w.Body)
	}
}

// TestSiteScoresNetworkParam: ?network= narrows the scope to that plane's
// ID; an unknown name is a 404, and a scoped session naming a foreign plane
// gets the byte-identical 404 — existence must not leak.
func TestSiteScoresNetworkParam(t *testing.T) {
	f := newFakeDB()
	tenant := store.NetworkAdminInfo{ID: uuid.New(), Name: "tenant-a", DisplayName: "Tenant A", CreatedAt: time.Now()}
	f.networks = append(f.networks, tenant)
	h := newTestAPI(t, f)

	get := func(cookie *http.Cookie, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.AddCookie(cookie)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	admin, _ := testSession(f, store.RoleAdmin, nil)
	w := get(admin, "/api/v1/sites/scores?network=tenant-a")
	if w.Code != http.StatusOK {
		t.Fatalf("admin ?network=tenant-a = %d %s", w.Code, w.Body)
	}
	if got := f.scopeArgs["SiteScores"]; len(got) != 1 || got[0] != tenant.ID {
		t.Errorf("scope = %v, want [%s]", got, tenant.ID)
	}
	if !strings.Contains(w.Body.String(), `"network":"tenant-a"`) {
		t.Errorf("response must echo the narrowing: %s", w.Body)
	}
	unknown := get(admin, "/api/v1/sites/scores?network=nope")
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown plane = %d, want 404: %s", unknown.Code, unknown.Body)
	}

	scoped, _ := testSession(f, store.RoleNetworkViewer, []store.NetworkRef{{ID: uuid.New(), Name: "tenant-b"}})
	foreign := get(scoped, "/api/v1/sites/scores?network=tenant-a")
	if foreign.Code != http.StatusNotFound {
		t.Fatalf("foreign plane = %d, want 404: %s", foreign.Code, foreign.Body)
	}
	// Both use the unknown-network shape (the pair endpoint's guard test
	// pins the same wording), so a tenant cannot tell foreign from absent.
	var a, b struct {
		Error string `json:"error"`
	}
	json.Unmarshal(foreign.Body.Bytes(), &a)
	json.Unmarshal(unknown.Body.Bytes(), &b)
	if !strings.Contains(a.Error, `"tenant-a" does not exist`) || !strings.Contains(b.Error, `"nope" does not exist`) {
		t.Errorf("404 bodies must use the unknown-network shape: %q / %q", a.Error, b.Error)
	}
}
