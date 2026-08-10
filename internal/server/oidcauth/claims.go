package oidcauth

import (
	"fmt"
	"slices"
)

// ClaimsError marks a token that verified cryptographically but did not
// carry a usable identity — the callback surfaces these as a configuration
// problem (sso-error=claims), not an IdP outage.
type ClaimsError struct{ msg string }

func (e *ClaimsError) Error() string { return e.msg }

func claimsErrorf(format string, args ...any) error {
	return &ClaimsError{msg: fmt.Sprintf(format, args...)}
}

// mapClaims turns verified raw claims into the dashboard identity. The
// username claim is required and must be a non-empty string (fail loud,
// naming the claim). The role claim may be a JSON string or array of
// strings — Keycloak's groups claim is an array — and an exact match
// against adminValues grants admin; anything else, including an absent
// claim, is a viewer. Viewer is the floor, never an error: role mapping
// can only elevate.
func mapClaims(usernameClaim, roleClaim string, adminValues []string, subject string, all map[string]any) (*Claims, error) {
	if subject == "" {
		return nil, claimsErrorf("id_token has an empty subject")
	}
	username, _ := all[usernameClaim].(string)
	if username == "" {
		return nil, claimsErrorf("id_token is missing username claim %q (or it is empty / not a string)", usernameClaim)
	}

	role := "viewer"
	if matchesAdmin(all[roleClaim], adminValues) {
		role = "admin"
	}
	return &Claims{Subject: subject, Username: username, Role: role}, nil
}

func matchesAdmin(claim any, adminValues []string) bool {
	if len(adminValues) == 0 {
		return false
	}
	switch v := claim.(type) {
	case string:
		return slices.Contains(adminValues, v)
	case []any:
		for _, e := range v {
			if s, ok := e.(string); ok && slices.Contains(adminValues, s) {
				return true
			}
		}
	}
	return false
}
