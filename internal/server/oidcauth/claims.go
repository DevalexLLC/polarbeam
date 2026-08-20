package oidcauth

import (
	"fmt"
	"slices"

	"github.com/devalexllc/polarbeam/internal/server/store"
)

// ClaimsError marks a token that verified cryptographically but did not
// carry a usable identity — the callback surfaces these as a configuration
// problem (sso-error=claims), not an IdP outage.
type ClaimsError struct{ msg string }

func (e *ClaimsError) Error() string { return e.msg }

func claimsErrorf(format string, args ...any) error {
	return &ClaimsError{msg: fmt.Sprintf(format, args...)}
}

// AccessDeniedError marks a token that verified and mapped cleanly but whose
// identity is not allowed in: unmatched_role is "deny" and the role claim
// matched neither admin_values nor any role rule. The callback surfaces it
// as sso-error=denied — a policy outcome, not a configuration problem.
type AccessDeniedError struct{ msg string }

func (e *AccessDeniedError) Error() string { return e.msg }

// mapClaims turns verified raw claims into the dashboard identity. The
// username claim is required and must be a non-empty string (fail loud,
// naming the claim). The role claim may be a JSON string or array of
// strings — Keycloak's groups claim is an array. Resolution order:
//
//  1. An exact match against adminValues grants the global admin role —
//     always, no rule can demote it.
//  2. Otherwise every matching role rule contributes: the strongest role
//     wins (network_admin > network_viewer) and the networks of all rules
//     granting it are unioned into Claims.Networks (names — the callback
//     resolves them against the networks table).
//  3. Otherwise unmatchedRole: "viewer" keeps the pre-tenancy floor (any
//     authenticated user is a global viewer), "deny" refuses the login
//     with AccessDeniedError.
func mapClaims(usernameClaim, roleClaim string, adminValues []string, roleRules []store.OIDCRoleRule,
	unmatchedRole, issuer, subject string, all map[string]any) (*Claims, error) {
	if issuer == "" {
		return nil, claimsErrorf("id_token has an empty issuer")
	}
	if subject == "" {
		return nil, claimsErrorf("id_token has an empty subject")
	}
	username, _ := all[usernameClaim].(string)
	if username == "" {
		return nil, claimsErrorf("id_token is missing username claim %q (or it is empty / not a string)", usernameClaim)
	}
	c := &Claims{Issuer: issuer, Subject: subject, Username: username}

	values := claimValues(all[roleClaim])
	if len(adminValues) > 0 && slices.ContainsFunc(values, func(v string) bool {
		return slices.Contains(adminValues, v)
	}) {
		c.Role = store.RoleAdmin
		return c, nil
	}

	for _, want := range []string{store.RoleNetworkAdmin, store.RoleNetworkViewer} {
		var networks []string
		for _, rule := range roleRules {
			if rule.Role != want || !slices.Contains(values, rule.Value) {
				continue
			}
			for _, n := range rule.Networks {
				if !slices.Contains(networks, n) {
					networks = append(networks, n)
				}
			}
		}
		if len(networks) > 0 {
			slices.Sort(networks)
			c.Role, c.Networks = want, networks
			return c, nil
		}
	}

	if unmatchedRole == "deny" {
		return nil, &AccessDeniedError{msg: fmt.Sprintf(
			"subject %s matched no admin value or role rule and unmatched_role is deny (role claim %q carried %q)",
			subject, roleClaim, values)}
	}
	c.Role = store.RoleViewer
	return c, nil
}

// claimValues normalizes a role claim to its string values: a JSON string
// is one value, an array keeps its string elements, anything else is empty.
func claimValues(claim any) []string {
	switch v := claim.(type) {
	case string:
		return []string{v}
	case []any:
		var out []string
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
