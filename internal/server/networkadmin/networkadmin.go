// Package networkadmin holds network-configuration validation shared by the
// admin CLI (network create/set) and the HTTP API (config/networks), so the
// two surfaces can never disagree about what a valid network looks like.
package networkadmin

import (
	"fmt"
	"strings"
)

// FieldNames carries the surface-specific spelling of each field so one
// validator produces natural error text for both the CLI ("--name") and the
// HTTP API ("name").
type FieldNames struct {
	Name string
}

// ValidateName rejects empty or whitespace-only network names, returning
// every problem (settings.go convention).
func ValidateName(name string, f FieldNames) []string {
	var problems []string
	if strings.TrimSpace(name) == "" {
		problems = append(problems, fmt.Sprintf("%s is required", f.Name))
	}
	return problems
}
