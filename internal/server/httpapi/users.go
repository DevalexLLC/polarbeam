// Admin account inventory: every dashboard user (local and SSO, including
// deleted identities reconstructed from the sign-in audit log) with its
// login-event aggregates, plus zero-filled monthly sign-in totals for the
// Settings -> Users activity chart. The list is filtered and paged
// server-side — OIDC JIT provisioning makes it the one config table that
// grows with people rather than infrastructure.

package httpapi

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

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
		Role:   oneOf(&problems, "role", q.Get("role"), "admin", "viewer"),
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
			CreatedAt: acc.CreatedAt,
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
