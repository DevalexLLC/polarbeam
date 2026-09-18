// Audit plumbing shared by every handler: the status-capturing writer and
// the one record withSession emits per request, the actor derived from the
// session, the scoped-404 helper, and the per-session "ended" records.
//
// The design rule is one record per request, emitted by the middleware,
// with handlers contributing only what the route pattern cannot express
// (audit.Add) or the reason a deliberately indistinguishable 404 was in
// fact a scope denial (audit.Deny). Handlers that own a richer record —
// login, logout, the syslog settings write — emit it themselves and call
// audit.Suppress so the generic one is not duplicated.

package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/devalexllc/polarbeam/internal/audit"
	"github.com/devalexllc/polarbeam/internal/server/store"
)

// statusWriter records the status a handler wrote. Handlers write exactly
// one response through writeJSON/writeError/http.Redirect, so the first
// WriteHeader (or an implicit 200 on the first Write) is the status.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// actorFrom is the audit identity of a session: username, auth source,
// and the session row id (never the token).
func actorFrom(s *store.SessionInfo) audit.Actor {
	if s == nil {
		return audit.Actor{}
	}
	return audit.Actor{User: s.Username, Source: s.AuthSource, Session: s.ID.String()}
}

// auditRequest emits the record a session-authenticated request owes once
// the handler has returned: every mutating request gets an api.write
// (whatever its outcome), a read gets an authz.denied only when it was
// refused. A 401/403, or a 404 the handler marked as a scope denial, is
// "denied"; any other 4xx/5xx is "failure". Successful reads are
// deliberately not logged — this is an audit trail, not an access log.
func (a *api) auditRequest(r *http.Request, s *store.SessionInfo, status int, ex *audit.Extras) {
	extras, denied, failed, suppressed := ex.Take()
	if suppressed {
		return
	}
	mutating := r.Method != http.MethodGet && r.Method != http.MethodHead
	outcome := audit.Success
	switch {
	case failed:
		outcome = audit.Failure
	case status == http.StatusUnauthorized || status == http.StatusForbidden || denied:
		outcome = audit.Denied
	case status >= 400:
		outcome = audit.Failure
	}
	if !mutating && outcome != audit.Denied {
		return
	}
	id, msg := audit.EventAPIWrite, "dashboard write"
	if !mutating {
		id, msg = audit.EventAuthzDenied, "dashboard read refused"
	}
	attrs := make([]slog.Attr, 0, 3+len(extras))
	attrs = append(attrs, routeAttrs(r)...)
	attrs = append(attrs, slog.Int("status", status))
	attrs = append(attrs, extras...)
	a.audit.Emit(r.Context(), audit.Event{
		ID: id, Msg: msg, Outcome: outcome,
		Actor: actorFrom(s), Remote: clientIP(r), Attrs: attrs,
	})
}

// routeAttrs names the request by its mux pattern (the event's "type"
// within api.write) and the concrete path (which carries the ids and names
// of the affected objects for PUT/DELETE routes).
func routeAttrs(r *http.Request) []slog.Attr {
	return []slog.Attr{slog.String("route", r.Pattern), slog.String("path", r.URL.Path)}
}

// writeUnscopedStoreError is writeStoreError for a scope-blind lookup on a
// scoped route (GetProbeConfig, used by probe PUT/DELETE before the
// handler proves scope itself): its ErrNotFound is a genuine miss, and the
// record says so explicitly so a 404 on a scoped route is never
// unclassified.
func writeUnscopedStoreError(w http.ResponseWriter, r *http.Request, what string, err error) {
	if errors.Is(err, store.ErrNotFound) {
		audit.Add(r.Context(), slog.String("reason", "not_found"))
	}
	writeStoreError(w, what, err)
}

// writeScopedStoreError is writeStoreError for calls that fold the
// caller's network scope into the query: their ErrNotFound means "missing
// or on a plane you cannot see", which the response must not distinguish
// but the audit record does, as a denial with that exact ambiguity named.
func writeScopedStoreError(w http.ResponseWriter, r *http.Request, what string, err error) {
	if errors.Is(err, store.ErrNotFound) {
		audit.Deny(r.Context(), "not_found_or_out_of_scope")
	}
	writeStoreError(w, what, err)
}

// sessionsEnded records one auth.session.ended per revoked session, after
// the store transaction that removed them committed. reason says why
// (logout, password_changed, password_reset, user_deleted,
// oidc_policy_changed, user_disabled); remote is the request that caused
// it, which for a revocation is the admin's or the owner's, not the
// session's.
//
// A revocation's DELETE also sweeps rows that had already expired but
// not yet been cleaned up. Those sessions ended at their expiry, not now
// and not for this reason, and the row is gone so nothing later can say
// so — they are recorded as expired here, at their ExpiresAt.
func (a *api) sessionsEnded(ctx context.Context, refs []store.SessionRef, reason, remote string) {
	now := time.Now()
	for _, ref := range refs {
		if !ref.ExpiresAt.After(now) {
			a.sessionsExpired(ctx, []store.SessionRef{ref}, now)
			continue
		}
		a.audit.Emit(ctx, audit.Event{
			ID: audit.EventSessionEnded, Msg: "session ended", Outcome: audit.Success,
			Actor:  audit.Actor{User: ref.Username, Session: ref.ID.String()},
			Remote: remote,
			Attrs:  []slog.Attr{slog.String("reason", reason)},
		})
	}
}

// sessionsExpired records sessions removed by the opportunistic cleanup.
// The record's time is the session's expiry — the moment access actually
// ended — with the cleanup time alongside, since the sweep can run days
// later.
func (a *api) sessionsExpired(ctx context.Context, refs []store.SessionRef, observed time.Time) {
	for _, ref := range refs {
		a.audit.Emit(ctx, audit.Event{
			ID: audit.EventSessionEnded, Msg: "session ended", Outcome: audit.Success,
			Actor: audit.Actor{User: ref.Username, Session: ref.ID.String()},
			At:    ref.ExpiresAt,
			Attrs: []slog.Attr{
				slog.String("reason", "expired"),
				slog.String("observed_at", observed.UTC().Format(time.RFC3339)),
			},
		})
	}
}

// reasonToken turns a human failure phrase ("state mismatch") into the
// snake_case token the record carries, so downstream key=value extraction
// never has to cope with spaces.
func reasonToken(what string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(what)), " ", "_")
}
