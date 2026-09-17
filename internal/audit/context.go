package audit

import (
	"context"
	"log/slog"
	"sync"
)

// Extras is the per-request audit scratchpad the HTTP middleware places
// in the context: handlers add the attributes only they know (the name of
// the object a POST created, the reason a 404 was really a scope denial),
// and the middleware folds them into the one record it emits when the
// handler returns. Suppress tells the middleware the handler emitted its
// own record instead.
type Extras struct {
	mu         sync.Mutex
	attrs      []slog.Attr
	denied     bool
	failed     bool
	suppressed bool
}

type ctxKey int

const extrasKey ctxKey = 0

// WithExtras returns a context carrying a fresh Extras and the Extras.
func WithExtras(ctx context.Context) (context.Context, *Extras) {
	ex := &Extras{}
	return context.WithValue(ctx, extrasKey, ex), ex
}

func extrasFrom(ctx context.Context) *Extras {
	ex, _ := ctx.Value(extrasKey).(*Extras)
	return ex
}

// Add appends attributes to the request's record. Outside a request (no
// Extras in ctx) it is a no-op, so store-facing helpers can call it
// unconditionally.
func Add(ctx context.Context, attrs ...slog.Attr) {
	if ex := extrasFrom(ctx); ex != nil {
		ex.mu.Lock()
		ex.attrs = append(ex.attrs, attrs...)
		ex.mu.Unlock()
	}
}

// Deny records that the request was refused for reason (out_of_scope,
// not_found_or_out_of_scope, ...) even though the HTTP status alone —
// a deliberately indistinguishable 404 — would not say so. The middleware
// classifies the record as denied.
func Deny(ctx context.Context, reason string) {
	if ex := extrasFrom(ctx); ex != nil {
		ex.mu.Lock()
		ex.attrs = append(ex.attrs, slog.String("reason", reason))
		ex.denied = true
		ex.mu.Unlock()
	}
}

// Fail records that the request failed for reason even though its HTTP
// status reads as a refusal — the self-service password change answers
// 403 to a wrong current password so the SPA does not treat it as session
// death, but the caller is authenticated and simply failed the check.
func Fail(ctx context.Context, reason string) {
	if ex := extrasFrom(ctx); ex != nil {
		ex.mu.Lock()
		ex.attrs = append(ex.attrs, slog.String("reason", reason))
		ex.failed = true
		ex.mu.Unlock()
	}
}

// Suppress tells the middleware not to emit its generic record for this
// request because the handler emitted a more specific one.
func Suppress(ctx context.Context) {
	if ex := extrasFrom(ctx); ex != nil {
		ex.mu.Lock()
		ex.suppressed = true
		ex.mu.Unlock()
	}
}

// Take returns the collected attributes and flags. Safe on a nil Extras.
func (ex *Extras) Take() (attrs []slog.Attr, denied, failed, suppressed bool) {
	if ex == nil {
		return nil, false, false, false
	}
	ex.mu.Lock()
	defer ex.mu.Unlock()
	return ex.attrs, ex.denied, ex.failed, ex.suppressed
}
