// Package configadmin holds the site, network, and external-target
// validation shared by the admin CLI (site set, network create/set, target
// add) and the HTTP API (config/sites, config/networks, config/targets), so
// the two surfaces can never disagree about what a valid value looks like.
// Both-or-neither coordinate presence stays surface-specific (flag presence
// vs JSON nulls); the rules here cover what a value means once it is
// present. Probe-config validation lives in probeadmin.
package configadmin

import (
	"fmt"
	"strings"
)

// Each *Fields type carries the surface-specific spelling of the fields so
// one validator produces natural error text for both the CLI ("--lat") and
// the HTTP API ("latitude").

// SiteFields names site fields for one surface.
type SiteFields struct {
	Name string
	Lat  string
	Lon  string
}

// ValidateSiteName rejects empty or whitespace-only site names, returning
// every problem (settings.go convention).
func ValidateSiteName(name string, f SiteFields) []string {
	return requiredName(name, f.Name)
}

// ValidateSiteCoords rejects out-of-range map positions. 0,0 is a real
// coordinate (off Ghana) and is valid; callers decide presence before
// calling.
func ValidateSiteCoords(lat, lon float64, f SiteFields) []string {
	var problems []string
	if lat < -90 || lat > 90 {
		problems = append(problems, fmt.Sprintf("%s must be between -90 and 90, got %g", f.Lat, lat))
	}
	if lon < -180 || lon > 180 {
		problems = append(problems, fmt.Sprintf("%s must be between -180 and 180, got %g", f.Lon, lon))
	}
	return problems
}

// NetworkFields names network fields for one surface.
type NetworkFields struct {
	Name string
}

// ValidateNetworkName rejects empty or whitespace-only network names,
// returning every problem (settings.go convention).
func ValidateNetworkName(name string, f NetworkFields) []string {
	return requiredName(name, f.Name)
}

// TargetFields names external-target fields for one surface.
type TargetFields struct {
	Name    string
	Address string
	URL     string
	Port    string
}

// ValidateTarget returns every problem with a prospective target
// (settings.go convention). Port 0 is the "unset" sentinel — ICMP/traceroute
// ignore the port, DNS reads 0 as "default 53" and NTP as "default 123" —
// so the legal range is 0–65535.
// Port is int64 so CLI callers can validate before narrowing to the wire's
// int32, instead of silently truncating.
func ValidateTarget(name, address, url string, port int64, f TargetFields) []string {
	problems := requiredName(name, f.Name)
	if strings.TrimSpace(address) == "" && strings.TrimSpace(url) == "" {
		problems = append(problems, fmt.Sprintf("%s or %s is required", f.Address, f.URL))
	}
	if port < 0 || port > 65535 {
		problems = append(problems, fmt.Sprintf("%s must be between 0 and 65535, got %d", f.Port, port))
	}
	return problems
}

// requiredName is the shared required-string check; field is the surface's
// spelling of the field name.
func requiredName(name, field string) []string {
	var problems []string
	if strings.TrimSpace(name) == "" {
		problems = append(problems, fmt.Sprintf("%s is required", field))
	}
	return problems
}
