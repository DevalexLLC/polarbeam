// Package siteadmin holds site-configuration validation shared by the admin
// CLI (site set) and the HTTP API (config/sites), so the two surfaces can
// never disagree about what a valid site looks like. Both-or-neither
// coordinate presence stays surface-specific (flag presence vs JSON nulls);
// the rules here cover what a value means once it is present.
package siteadmin

import (
	"fmt"
	"strings"
)

// FieldNames carries the surface-specific spelling of each field so one
// validator produces natural error text for both the CLI ("--lat") and the
// HTTP API ("latitude").
type FieldNames struct {
	Name string
	Lat  string
	Lon  string
}

// ValidateName rejects empty or whitespace-only site names, returning every
// problem (settings.go convention).
func ValidateName(name string, f FieldNames) []string {
	var problems []string
	if strings.TrimSpace(name) == "" {
		problems = append(problems, fmt.Sprintf("%s is required", f.Name))
	}
	return problems
}

// ValidateCoords rejects out-of-range map positions. 0,0 is a real
// coordinate (off Ghana) and is valid; callers decide presence before
// calling.
func ValidateCoords(lat, lon float64, f FieldNames) []string {
	var problems []string
	if lat < -90 || lat > 90 {
		problems = append(problems, fmt.Sprintf("%s must be between -90 and 90, got %g", f.Lat, lat))
	}
	if lon < -180 || lon > 180 {
		problems = append(problems, fmt.Sprintf("%s must be between -180 and 180, got %g", f.Lon, lon))
	}
	return problems
}
