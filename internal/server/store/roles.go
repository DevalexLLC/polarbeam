package store

import "slices"

// Dashboard roles. Admin and viewer are global: admin writes everything,
// viewer reads everything. The network_* roles are tenant roles scoped to
// an explicit set of networks (user_networks): network_viewer reads only
// its planes; network_admin additionally gets the network-scoped writes
// (probes, meshes, targets, tokens, thresholds — landing with the scoped
// write surface). requireRole("admin") is an exact string compare, so the
// scoped roles are denied every global-admin endpoint by construction —
// never add a role hierarchy that would soften that.
const (
	RoleAdmin         = "admin"
	RoleViewer        = "viewer"
	RoleNetworkAdmin  = "network_admin"
	RoleNetworkViewer = "network_viewer"
)

// Roles lists every valid users.role value, matching the users_role_check
// constraint (migration 0018).
var Roles = []string{RoleAdmin, RoleViewer, RoleNetworkAdmin, RoleNetworkViewer}

// ValidRole reports whether role is one of the four dashboard roles.
func ValidRole(role string) bool { return slices.Contains(Roles, role) }

// RoleIsNetworkScoped reports whether role's visibility is limited to the
// user's user_networks set.
func RoleIsNetworkScoped(role string) bool {
	return role == RoleNetworkAdmin || role == RoleNetworkViewer
}
