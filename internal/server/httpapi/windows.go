package httpapi

import (
	"time"

	"github.com/devalexllc/polarbeam/internal/server/store"
)

// windowSpec maps a query window to its bucket width and the table that
// serves it (M5): short windows read raw probe_results at full resolution
// (raw retention is 14d, comfortably above 7d), long windows read the
// hourly/daily continuous aggregates. 24h is served beyond the documented
// 7d..365d list so a freshly seeded stack has a live chart to look at; it
// rides the identical code path.
type windowSpec struct {
	Window time.Duration
	Bucket time.Duration
	Source store.Source
}

var windows = map[string]windowSpec{
	"24h":  {24 * time.Hour, time.Minute, store.SourceRaw},
	"7d":   {7 * 24 * time.Hour, 5 * time.Minute, store.SourceRaw},
	"30d":  {30 * 24 * time.Hour, time.Hour, store.SourceHourly},
	"90d":  {90 * 24 * time.Hour, 3 * time.Hour, store.SourceHourly},
	"365d": {365 * 24 * time.Hour, 24 * time.Hour, store.SourceDaily},
}

// parseWindow resolves a ?window= value; ok is false for anything not in
// the table (the handler answers 400, never a silent default).
func parseWindow(s string) (windowSpec, bool) {
	if s == "" {
		s = "24h"
	}
	spec, ok := windows[s]
	return spec, ok
}
