package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/devalexllc/polarbeam/internal/server/store"
)

// fakeBannerState backs the bannerStore fake methods (in httpapi_test.go).
type fakeBannerState struct {
	banner *store.BannerSettings
}

func putBanner(h http.Handler, cookie *http.Cookie, csrf string, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("PUT", "/api/v1/settings/ui-banner", strings.NewReader(body))
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

func getBanner(t *testing.T, h http.Handler, path string, cookie *http.Cookie) (*httptest.ResponseRecorder, uiBannerJSON) {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var got uiBannerJSON
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("bad %s body: %v", path, err)
		}
	}
	return w, got
}

func TestUIBannerOpenRead(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)

	// No session: defaults are disabled and empty.
	w, got := getBanner(t, h, "/api/v1/ui-banner", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("open GET = %d, want 200: %s", w.Code, w.Body)
	}
	if got.Enabled || got.Text != "" {
		t.Errorf("default banner = %+v, want disabled/empty", got)
	}

	// Staged text behind a disabled banner must never leak pre-auth.
	f.banner = &store.BannerSettings{Enabled: false, Text: "STAGED"}
	if _, got = getBanner(t, h, "/api/v1/ui-banner", nil); got.Text != "" {
		t.Errorf("disabled banner leaked text %q", got.Text)
	}

	f.banner = &store.BannerSettings{Enabled: true, Text: "PROPRIETARY"}
	if _, got = getBanner(t, h, "/api/v1/ui-banner", nil); !got.Enabled || got.Text != "PROPRIETARY" {
		t.Errorf("enabled banner = %+v", got)
	}
}

func TestBannerSettingsRequireAuth(t *testing.T) {
	h := newTestAPI(t, newFakeDB())
	for _, m := range []string{"GET", "PUT"} {
		req := httptest.NewRequest(m, "/api/v1/settings/ui-banner", strings.NewReader("{}"))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s without session = %d, want 401", m, w.Code)
		}
	}
}

func TestBannerSettingsViewerForbidden(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, csrf := loginAndCookie(t, h, f) // viewer

	// Reads are admin-only too (updated_by usernames are admin material).
	req := httptest.NewRequest("GET", "/api/v1/settings/ui-banner", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("viewer GET = %d, want 403", w.Code)
	}

	w = putBanner(h, cookie, csrf, `{"enabled":true,"text":"PROPRIETARY"}`)
	if w.Code != http.StatusForbidden {
		t.Errorf("viewer PUT = %d, want 403: %s", w.Code, w.Body)
	}
	if f.banner != nil {
		t.Error("viewer PUT must not write banner settings")
	}
}

func TestBannerSettingsRoundTrip(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, csrf := loginRole(t, h, f, "root", "admin")

	// GET before any write serves the seeded defaults.
	req := httptest.NewRequest("GET", "/api/v1/settings/ui-banner", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET = %d: %s", w.Code, w.Body)
	}
	var got bannerSettingsJSON
	json.Unmarshal(w.Body.Bytes(), &got)
	if got.Enabled || got.Text != "" {
		t.Errorf("defaults = %+v, want disabled/empty", got)
	}

	// PUT trims and stamps updated_by/updated_at.
	w = putBanner(h, cookie, csrf, `{"enabled":true,"text":"  PROPRIETARY  "}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", w.Code, w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad PUT body: %v", err)
	}
	if !got.Enabled || got.Text != "PROPRIETARY" {
		t.Errorf("PUT echo = %+v, want trimmed PROPRIETARY", got)
	}
	if got.UpdatedBy != "root" {
		t.Errorf("updated_by = %q, want the session username", got.UpdatedBy)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("updated_at not stamped")
	}

	// The open read now serves it.
	if _, open := getBanner(t, h, "/api/v1/ui-banner", nil); !open.Enabled || open.Text != "PROPRIETARY" {
		t.Errorf("open GET after PUT = %+v", open)
	}

	// Disabling redacts the (kept) text on the open read only.
	w = putBanner(h, cookie, csrf, `{"enabled":false,"text":"PROPRIETARY"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("disable PUT = %d: %s", w.Code, w.Body)
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	if got.Enabled || got.Text != "PROPRIETARY" {
		t.Errorf("admin view after disable = %+v, want staged text kept", got)
	}
	if _, open := getBanner(t, h, "/api/v1/ui-banner", nil); open.Enabled || open.Text != "" {
		t.Errorf("open GET after disable = %+v, want disabled/empty", open)
	}
}

func TestBannerSettingsPutValidation(t *testing.T) {
	f := newFakeDB()
	h := newTestAPI(t, f)
	cookie, csrf := loginRole(t, h, f, "root", "admin")

	cases := map[string]struct {
		body string
		want string // substring the error must name
	}{
		"enabled empty text": {`{"enabled":true,"text":""}`, "required"},
		"enabled blank text": {`{"enabled":true,"text":"   "}`, "required"},
		"over 300 chars":     {`{"enabled":true,"text":"` + strings.Repeat("x", 301) + `"}`, "at most 300"},
		"newline in text":    {`{"enabled":true,"text":"TOP\nSECRET"}`, "control characters"},
		"control char":       {`{"enabled":true,"text":"BEEP\u0007"}`, "control characters"},
		"unknown key":        {`{"enabled":true,"text":"ok","surprise":1}`, ""},
		"not json":           {`banner please`, ""},
	}
	for name, tc := range cases {
		w := putBanner(h, cookie, csrf, tc.body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: PUT = %d, want 400: %s", name, w.Code, w.Body)
		}
		if tc.want != "" && !strings.Contains(w.Body.String(), tc.want) {
			t.Errorf("%s: error %q does not name %q", name, w.Body, tc.want)
		}
		if f.banner != nil {
			t.Fatalf("%s: invalid PUT must not write banner settings", name)
		}
	}

	// 300 chars of multibyte runes is exactly at the limit (rune count, not
	// bytes — mirrors Postgres char_length).
	w := putBanner(h, cookie, csrf, `{"enabled":true,"text":"`+strings.Repeat("ü", 300)+`"}`)
	if w.Code != http.StatusOK {
		t.Errorf("300-rune multibyte text = %d, want 200: %s", w.Code, w.Body)
	}
}

func TestValidateBannerNamesEveryProblem(t *testing.T) {
	_, err := validateBannerSettings(bannerSettingsRequest{
		// Control char mid-string (a leading one would just be trimmed).
		Enabled: true, Text: "x\ax" + strings.Repeat("x", 301),
	})
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"control characters", "at most 300"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}
