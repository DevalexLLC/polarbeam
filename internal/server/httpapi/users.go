// Admin account inventory: every dashboard user (local and SSO, including
// deleted identities reconstructed from the sign-in audit log) with its
// login-event aggregates, plus zero-filled monthly sign-in totals for the
// Settings -> Users activity chart. The list is filtered and paged
// server-side — OIDC JIT provisioning makes it the one config table that
// grows with people rather than infrastructure.

package httpapi

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/devalexllc/polarbeam/internal/server/auth"
	"github.com/devalexllc/polarbeam/internal/server/store"
)

// loginMetricsMonths is the activity chart's window; the current UTC
// calendar month is always the last bucket.
const loginMetricsMonths = 12

const (
	usersDefaultLimit = 50
	usersMaxLimit     = 200
)

type userAccountJSON struct {
	ID          string     `json:"id"`
	Username    string     `json:"username"`
	Role        string     `json:"role"`
	AuthSource  string     `json:"auth_source"`
	Status      string     `json:"status"` // "active" | "disabled" | "deleted"
	LoginCount  int64      `json:"login_count"`
	LastLoginAt *time.Time `json:"last_login_at"` // null = never
	CreatedAt   *time.Time `json:"created_at"`    // null for deleted identities
	// Networks is the scope of a live network-scoped account; null for
	// global roles and deleted identities.
	Networks []string `json:"networks"`
}

type loginMonthJSON struct {
	// Month is the UTC calendar month as "2006-01" — formatted server-side
	// so a browser timezone can never shift a bucket's label.
	Month       string `json:"month"`
	Total       int64  `json:"total"`
	Local       int64  `json:"local"`
	OIDC        int64  `json:"oidc"`
	UniqueUsers int64  `json:"unique_users"`
}

// oneOf validates an enum-ish query parameter, collecting a problem string
// on mismatch. Empty means "any" and is always allowed.
func oneOf(problems *[]string, name, val string, allowed ...string) string {
	if val == "" {
		return ""
	}
	if slices.Contains(allowed, val) {
		return val
	}
	*problems = append(*problems, fmt.Sprintf("%s must be one of: %s", name, strings.Join(allowed, ", ")))
	return ""
}

func (a *api) handleUsersGet(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var problems []string
	f := store.UserAccountFilter{
		Query:  q.Get("q"),
		Role:   oneOf(&problems, "role", q.Get("role"), store.Roles...),
		Status: oneOf(&problems, "status", q.Get("status"), "active", "disabled", "deleted"),
		Source: oneOf(&problems, "source", q.Get("source"), "local", "oidc"),
		Limit:  usersDefaultLimit,
	}
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > usersMaxLimit {
			problems = append(problems, fmt.Sprintf("limit must be an integer between 1 and %d", usersMaxLimit))
		} else {
			f.Limit = n
		}
	}
	if s := q.Get("offset"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			problems = append(problems, "offset must be a non-negative integer")
		} else {
			f.Offset = n
		}
	}
	if len(problems) > 0 {
		writeError(w, http.StatusBadRequest, strings.Join(problems, "; "))
		return
	}

	accounts, total, err := a.db.ListUserAccounts(r.Context(), f)
	if err != nil {
		internalError(w, "list user accounts", err)
		return
	}
	months, err := a.db.MonthlyLoginStats(r.Context(), loginMetricsMonths)
	if err != nil {
		internalError(w, "monthly login stats", err)
		return
	}

	users := make([]userAccountJSON, 0, len(accounts))
	for _, acc := range accounts {
		users = append(users, userAccountJSON{
			ID: acc.ID.String(), Username: acc.Username, Role: acc.Role,
			AuthSource: acc.AuthSource, Status: acc.Status,
			LoginCount: acc.LoginCount, LastLoginAt: acc.LastLoginAt,
			CreatedAt: acc.CreatedAt, Networks: acc.Networks,
		})
	}
	monthsOut := make([]loginMonthJSON, 0, len(months))
	for _, m := range months {
		monthsOut = append(monthsOut, loginMonthJSON{
			Month: m.Month.UTC().Format("2006-01"), Total: m.Total,
			Local: m.Local, OIDC: m.OIDC, UniqueUsers: m.UniqueUsers,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"users":        users,
		"total":        total,
		"login_months": monthsOut,
	})
}

// handleUserPost creates a local account. The password is generated
// server-side and returned exactly once — no weak admin-chosen credentials,
// and the cleartext never touches the database (same posture as join
// tokens). SSO accounts are never created here: they JIT-provision at
// first login.
func (a *api) handleUserPost(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string   `json:"username"`
		Role     string   `json:"role"`
		Networks []string `json:"networks"`
	}
	if !decodeStrict(w, r, &in) {
		return
	}
	var problems []string
	if strings.TrimSpace(in.Username) == "" {
		problems = append(problems, "username is required")
	}
	if !store.ValidRole(in.Role) {
		problems = append(problems, "role must be one of: "+strings.Join(store.Roles, ", "))
	} else if store.RoleIsNetworkScoped(in.Role) && len(in.Networks) == 0 {
		problems = append(problems, "networks is required for role "+in.Role)
	} else if !store.RoleIsNetworkScoped(in.Role) && len(in.Networks) > 0 {
		problems = append(problems, "networks is only valid for the network-scoped roles")
	}
	networkIDs, nprob, ok := a.resolveNetworkNames(w, r, in.Networks)
	if !ok {
		return
	}
	problems = append(problems, nprob...)
	if len(problems) > 0 {
		writeError(w, http.StatusBadRequest, strings.Join(problems, "; "))
		return
	}

	password, hash, ok := mintPassword(w)
	if !ok {
		return
	}
	id, err := a.db.CreateUser(r.Context(), strings.TrimSpace(in.Username), hash, in.Role, networkIDs)
	if err != nil {
		writeStoreError(w, "create user", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":       id.String(),
		"username": strings.TrimSpace(in.Username),
		"role":     in.Role,
		"networks": in.Networks,
		"password": password,
	})
}

// resolveNetworkNames maps network names to IDs, collecting a problem per
// unknown name (never silently dropping one — the scope is a trust
// decision, same posture as token minting). ok=false means an internal
// error was already written.
func (a *api) resolveNetworkNames(w http.ResponseWriter, r *http.Request, names []string) (ids []uuid.UUID, problems []string, ok bool) {
	for _, name := range names {
		id, err := a.db.NetworkIDByName(r.Context(), name)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				problems = append(problems, fmt.Sprintf("network %q does not exist", name))
				continue
			}
			internalError(w, "resolve network", err)
			return nil, nil, false
		}
		ids = append(ids, id)
	}
	return ids, problems, true
}

// mintPassword generates the shown-once password for admin-driven account
// flows (create, reset): 18 random bytes -> 24 base64url chars, comfortably
// past the 8-char minimum. Returns the cleartext and its argon2id hash; on
// failure the response has been written and ok is false.
func mintPassword(w http.ResponseWriter) (password, hash string, ok bool) {
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		internalError(w, "generate password", err)
		return "", "", false
	}
	password = base64.RawURLEncoding.EncodeToString(raw)
	hash, err := auth.HashPassword(password)
	if err != nil {
		internalError(w, "hash password", err)
		return "", "", false
	}
	return password, hash, true
}

// userIDParam parses the {id} path segment, refusing to act on the caller's
// own account with selfRefusal as the 409 message — self-disable locks the
// admin out mid-session, self-delete is never what anyone meant, and
// self-reset would kill the caller's own session mid-response.
func (a *api) userIDParam(w http.ResponseWriter, r *http.Request, selfRefusal string) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "user id must be a UUID")
		return uuid.Nil, false
	}
	if s := sessionFrom(r.Context()); s != nil && s.UserID == id {
		writeError(w, http.StatusConflict, selfRefusal)
		return uuid.Nil, false
	}
	return id, true
}

// handleUserResetPassword replaces a local user's password with a fresh
// server-generated one, returned exactly once (same posture as create), and
// signs out all of the target's sessions — the old credential was presumably
// lost or leaked. Deleted identities 404 and federated accounts 409 in the
// store; neither is ever a silent no-op.
func (a *api) handleUserResetPassword(w http.ResponseWriter, r *http.Request) {
	id, ok := a.userIDParam(w, r, "reset your own password from the user menu instead")
	if !ok {
		return
	}
	password, hash, ok := mintPassword(w)
	if !ok {
		return
	}
	username, role, err := a.db.ResetLocalUserPassword(r.Context(), id, hash)
	if err != nil {
		writeStoreError(w, "reset password", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"id":       id.String(),
		"username": username,
		"role":     role,
		"password": password,
	})
}

func (a *api) handleUserPut(w http.ResponseWriter, r *http.Request) {
	id, ok := a.userIDParam(w, r, "you cannot disable or delete your own account")
	if !ok {
		return
	}
	var in struct {
		Disabled *bool `json:"disabled"`
		// Networks replaces a local scoped user's network set. The store
		// refuses it for global roles and for federated accounts (whose
		// scope the IdP mapping re-derives on every login).
		Networks []string `json:"networks"`
	}
	if !decodeStrict(w, r, &in) {
		return
	}
	if in.Disabled == nil && in.Networks == nil {
		writeError(w, http.StatusBadRequest, "disabled or networks is required")
		return
	}
	if in.Networks != nil {
		networkIDs, problems, ok := a.resolveNetworkNames(w, r, in.Networks)
		if !ok {
			return
		}
		if len(in.Networks) == 0 {
			problems = append(problems, "networks must not be empty (a scoped user needs at least one network)")
		}
		if len(problems) > 0 {
			writeError(w, http.StatusBadRequest, strings.Join(problems, "; "))
			return
		}
		if err := a.db.SetUserNetworks(r.Context(), id, networkIDs); err != nil {
			writeStoreError(w, "set user networks", err)
			return
		}
	}
	if in.Disabled != nil {
		if err := a.db.SetUserDisabled(r.Context(), id, *in.Disabled); err != nil {
			writeStoreError(w, "set user disabled", err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{})
}

func (a *api) handleUserDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := a.userIDParam(w, r, "you cannot disable or delete your own account")
	if !ok {
		return
	}
	if err := a.db.DeleteUser(r.Context(), id); err != nil {
		writeStoreError(w, "delete user", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{})
}
