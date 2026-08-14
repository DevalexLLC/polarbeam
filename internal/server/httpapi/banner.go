package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/devalexllc/polarbeam/internal/server/store"
)

// maxBannerTextChars mirrors the char_length CHECK on banner_settings —
// counted in runes to match Postgres char_length semantics.
const maxBannerTextChars = 300

// uiBannerJSON is the open read: exactly what every visitor sees rendered.
type uiBannerJSON struct {
	Enabled bool   `json:"enabled"`
	Text    string `json:"text"`
}

type bannerSettingsJSON struct {
	Enabled   bool      `json:"enabled"`
	Text      string    `json:"text"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by"`
}

type bannerSettingsRequest struct {
	Enabled bool   `json:"enabled"`
	Text    string `json:"text"`
}

func toBannerSettingsJSON(b *store.BannerSettings) bannerSettingsJSON {
	return bannerSettingsJSON{
		Enabled:   b.Enabled,
		Text:      b.Text,
		UpdatedAt: b.UpdatedAt,
		UpdatedBy: b.UpdatedBy,
	}
}

// validateBannerSettings names every problem, not just the first — the SPA
// form mirrors these rules. Returns the trimmed text to store.
func validateBannerSettings(in bannerSettingsRequest) (string, error) {
	text := strings.TrimSpace(in.Text)
	var problems []string
	if strings.ContainsFunc(text, unicode.IsControl) {
		// The band is a single line; newlines and tabs are mistakes.
		problems = append(problems, "text must not contain control characters")
	}
	if utf8.RuneCountInString(text) > maxBannerTextChars {
		problems = append(problems, "text must be at most 300 characters")
	}
	if in.Enabled && text == "" {
		problems = append(problems, "text is required when the banner is enabled")
	}
	if len(problems) > 0 {
		return "", errors.New(strings.Join(problems, "; "))
	}
	return text, nil
}

// handleUIBannerGet is open: every visitor renders the banner, the sign-in
// screen included, so there is nothing to protect. Disabled banners never
// leak staged text.
func (a *api) handleUIBannerGet(w http.ResponseWriter, r *http.Request) {
	b, err := a.db.GetBannerSettings(r.Context())
	if err != nil {
		internalError(w, "get banner settings", err)
		return
	}
	out := uiBannerJSON{Enabled: b.Enabled}
	if b.Enabled {
		out.Text = b.Text
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *api) handleBannerSettingsGet(w http.ResponseWriter, r *http.Request) {
	b, err := a.db.GetBannerSettings(r.Context())
	if err != nil {
		internalError(w, "get banner settings", err)
		return
	}
	writeJSON(w, http.StatusOK, toBannerSettingsJSON(b))
}

func (a *api) handleBannerSettingsPut(w http.ResponseWriter, r *http.Request) {
	var in bannerSettingsRequest
	if !decodeStrict(w, r, &in) {
		return
	}
	text, err := validateBannerSettings(in)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s := sessionFrom(r.Context())
	out, err := a.db.UpdateBannerSettings(r.Context(), store.BannerSettings{
		Enabled:   in.Enabled,
		Text:      text,
		UpdatedBy: s.Username,
	})
	if err != nil {
		internalError(w, "update banner settings", err)
		return
	}
	writeJSON(w, http.StatusOK, toBannerSettingsJSON(out))
}
