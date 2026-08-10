// Package targetadmin holds external-target validation shared by the admin
// CLI (target add) and the HTTP API (config/targets), so the two surfaces can
// never disagree about what a valid target looks like.
package targetadmin

import (
	"fmt"
	"strings"
)

// FieldNames carries the surface-specific spelling of each field so one
// validator produces natural error text for both the CLI ("--port") and the
// HTTP API ("port").
type FieldNames struct {
	Name    string
	Address string
	URL     string
	Port    string
}

// Validate returns every problem with a prospective target (settings.go
// convention). Port 0 is the "unset" sentinel — ICMP/traceroute ignore the
// port and DNS reads 0 as "default 53" — so the legal range is 0–65535.
// Port is int64 so CLI callers can validate before narrowing to the wire's
// int32, instead of silently truncating.
func Validate(name, address, url string, port int64, f FieldNames) []string {
	var problems []string
	if strings.TrimSpace(name) == "" {
		problems = append(problems, fmt.Sprintf("%s is required", f.Name))
	}
	if strings.TrimSpace(address) == "" && strings.TrimSpace(url) == "" {
		problems = append(problems, fmt.Sprintf("%s or %s is required", f.Address, f.URL))
	}
	if port < 0 || port > 65535 {
		problems = append(problems, fmt.Sprintf("%s must be between 0 and 65535, got %d", f.Port, port))
	}
	return problems
}
