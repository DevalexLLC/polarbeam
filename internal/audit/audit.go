// Package audit is the server's security-event vocabulary: the one shape
// every login, denial, account change, configuration write, agent
// credential decision, and lifecycle transition is recorded in, so the
// stderr log and any forwarder downstream of it (RFC 5424 syslog) can
// tell an audit record from an operational message and can rely on the
// same field names in every record.
//
// An audit record is an ordinary slog record whose first attribute is
// the reserved key "event" (Event.ID, the record's type — MSGID on the
// wire). The remaining fixed attributes answer the questions NIST SP
// 800-53 AU-3 and the DISA ASD STIG ask of every record: outcome, the
// identity of the subject (user, user_source, session), and the source
// address (remote). Everything else rides as event-specific attributes.
//
// The reserved key is checked by a test in this package that scans the
// tree: nothing outside audit may log under "event", so a record carrying
// it is an audit record by construction.
package audit

import (
	"context"
	"log/slog"
	"runtime"
	"strings"
	"testing"
	"time"
)

// KeyEvent is the reserved attribute key that marks a record as an audit
// event. Handlers that forward or filter audit records key on it.
const KeyEvent = "event"

// Fixed attribute keys, in the order Emit writes them.
const (
	KeyOutcome    = "outcome"
	KeyUser       = "user"
	KeyUserSource = "user_source"
	KeySession    = "session"
	KeyRemote     = "remote"
)

// Outcome is the result of the audited action.
type Outcome string

const (
	// Success: the action completed.
	Success Outcome = "success"
	// Failure: the action was attempted and did not complete (bad
	// credentials, validation, internal error).
	Failure Outcome = "failure"
	// Denied: the caller was refused before the action ran (no session,
	// wrong role, out-of-scope resource, rate limit, revoked certificate).
	Denied Outcome = "denied"
)

// Actor identifies who performed the action. Source is one of local,
// oidc, cli, or agent; Session is the session row's UUID (never the token
// or its hash) so a session's creation, use, and end can be correlated.
type Actor struct {
	User    string
	Source  string
	Session string
}

// Actor sources.
const (
	SourceLocal = "local"
	SourceOIDC  = "oidc"
	SourceCLI   = "cli"
	SourceAgent = "agent"
)

// Event is one audit record. At is optional: when set it becomes the
// record's time (an expired session is recorded at its expiry, not at the
// cleanup that noticed it); zero means now.
type Event struct {
	ID      string
	Msg     string
	Outcome Outcome
	Actor   Actor
	Remote  string
	At      time.Time
	Attrs   []slog.Attr
}

// Logger emits audit records through a slog.Logger. A nil *Logger and a
// Logger built from a nil *slog.Logger both use slog.Default() at emit
// time, so callers constructed before logging is set up still reach the
// handler installed later.
type Logger struct {
	l *slog.Logger
}

// New returns a Logger writing through l (nil: slog.Default() per call).
func New(l *slog.Logger) *Logger { return &Logger{l: l} }

func (a *Logger) logger() *slog.Logger {
	if a != nil && a.l != nil {
		return a.l
	}
	return slog.Default()
}

// forbiddenKeys is the V-222444 fence ("must not write sensitive data into
// the application logs"): an attribute under any of these keys is refused.
// Matching is by whole key or by a "_"-delimited component, so client_secret,
// csrf_token, and key_pem are all caught without a list of every spelling.
var forbiddenKeys = []string{"password", "token", "secret", "cookie", "pem", "authorization", "csrf"}

// forbidden reports whether key names a value that must never be logged.
func forbidden(key string) bool {
	k := strings.ToLower(key)
	for _, part := range strings.Split(k, "_") {
		for _, bad := range forbiddenKeys {
			if part == bad {
				return true
			}
		}
	}
	return false
}

// Emit writes the record. Success logs at Info; Failure and Denied at
// Warn, so the default log.level of info carries every audit record and
// a warn-level sink still sees every refusal. An unknown event ID or a
// forbidden attribute key is a programming error: under `go test` it
// panics, in production the offending attribute is dropped (or the ID kept)
// and an error is logged so the mistake is visible, never silent.
func (a *Logger) Emit(ctx context.Context, ev Event) {
	l := a.logger()
	if _, ok := Catalog[ev.ID]; !ok {
		if testing.Testing() {
			panic("audit: event " + ev.ID + " is not in the catalog")
		}
		l.Error("audit: event id not in catalog", "id", ev.ID)
	}
	level := slog.LevelInfo
	if ev.Outcome != Success {
		level = slog.LevelWarn
	}
	if !l.Enabled(ctx, level) {
		return
	}
	attrs := make([]slog.Attr, 0, 6+len(ev.Attrs))
	attrs = append(attrs, slog.String(KeyEvent, ev.ID), slog.String(KeyOutcome, string(ev.Outcome)))
	if ev.Actor.User != "" {
		attrs = append(attrs, slog.String(KeyUser, ev.Actor.User))
	}
	if ev.Actor.Source != "" {
		attrs = append(attrs, slog.String(KeyUserSource, ev.Actor.Source))
	}
	if ev.Actor.Session != "" {
		attrs = append(attrs, slog.String(KeySession, ev.Actor.Session))
	}
	if ev.Remote != "" {
		attrs = append(attrs, slog.String(KeyRemote, ev.Remote))
	}
	for _, at := range ev.Attrs {
		if forbidden(at.Key) {
			if testing.Testing() {
				panic("audit: attribute " + at.Key + " names a secret and must not be logged")
			}
			l.Error("audit: dropped attribute that names a secret", "id", ev.ID, "key", at.Key)
			continue
		}
		attrs = append(attrs, at)
	}
	at := ev.At
	if at.IsZero() {
		at = time.Now()
	}
	var pcs [1]uintptr
	runtime.Callers(2, pcs[:])
	rec := slog.NewRecord(at, level, ev.Msg, pcs[0])
	rec.AddAttrs(attrs...)
	_ = l.Handler().Handle(ctx, rec)
}

// IsAudit reports whether a slog record is an audit record — its first
// attribute is the reserved event key. Sinks that filter or classify
// records (the syslog forwarder) use it.
func IsAudit(r slog.Record) bool {
	found := false
	r.Attrs(func(at slog.Attr) bool {
		found = at.Key == KeyEvent
		return false // only the first attribute matters
	})
	return found
}

// EventID returns the record's event id, or "" for a non-audit record.
func EventID(r slog.Record) string {
	id := ""
	r.Attrs(func(at slog.Attr) bool {
		if at.Key == KeyEvent {
			id = at.Value.String()
		}
		return false
	})
	return id
}
