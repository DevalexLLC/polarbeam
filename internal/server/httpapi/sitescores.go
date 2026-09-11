package httpapi

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
)

// siteScoreJSON is one site's month-to-date tallies. Counts, not ratios:
// the SPA derives availability (ok_samples / samples) and performance
// (healthy_ok_samples / ok_samples) itself, so a zero denominator renders
// as "no data" rather than an invented 100 %.
type siteScoreJSON struct {
	Name             string `json:"name"`
	Samples          int64  `json:"samples"`
	OKSamples        int64  `json:"ok_samples"`
	HealthyOKSamples int64  `json:"healthy_ok_samples"`
}

type siteScoresJSON struct {
	// Month is the UTC calendar month the tallies cover, "2006-01" —
	// formatted server-side like the login-metrics months so a browser
	// time zone can never shift the label off the window it describes.
	Month string    `json:"month"`
	Since time.Time `json:"since"`
	AsOf  time.Time `json:"as_of"`
	// Network echoes the applied ?network= narrowing ("" = every plane the
	// session can see).
	Network string          `json:"network"`
	Sites   []siteScoreJSON `json:"sites"`
}

// monthStartUTC is the first instant of now's UTC calendar month. Pure so
// the boundary rule is unit-testable: a local date that is still last month
// must not move the window, and neither must a non-UTC now.
func monthStartUTC(now time.Time) time.Time {
	now = now.UTC()
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// handleSiteScores serves the map card's month-to-date availability and
// performance tallies for every visible site in one read (the card can show
// any site on hover, so the SPA needs them all at once). Scope is the
// session's; an explicit ?network= narrows it through the same guard the
// list endpoints use, so an unknown or out-of-scope plane is a loud 404
// worded exactly like a name that never existed. Traceroute is excluded the
// way the fleet health strip excludes it: its run-accounting rows would
// poison a success ratio.
func (a *api) handleSiteScores(w http.ResponseWriter, r *http.Request) {
	scope := scopeIDs(r.Context())
	network := r.URL.Query().Get("network")
	if network != "" {
		id, ok := a.requireNetworkScopeName(w, r, network)
		if !ok {
			return
		}
		scope = []uuid.UUID{id}
	}
	now := time.Now().UTC()
	since := monthStartUTC(now)
	rows, err := a.db.SiteScores(r.Context(), since, int16(pb.ProbeType_PROBE_TYPE_TRACEROUTE), scope)
	if err != nil {
		internalError(w, "site scores", err)
		return
	}
	sites := make([]siteScoreJSON, 0, len(rows))
	for _, row := range rows {
		sites = append(sites, siteScoreJSON{
			Name: row.Name, Samples: row.Samples, OKSamples: row.OK, HealthyOKSamples: row.HealthyOK,
		})
	}
	writeJSON(w, http.StatusOK, siteScoresJSON{
		Month: since.Format("2006-01"), Since: since, AsOf: now, Network: network, Sites: sites,
	})
}
