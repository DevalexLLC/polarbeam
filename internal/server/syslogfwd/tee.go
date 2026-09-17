package syslogfwd

import (
	"context"
	"errors"
	"log/slog"
)

// tee fans one record out to two handlers, consulting each one's Enabled
// so a level the stderr handler filters can still reach the forwarder
// (audit records are always eligible there) and vice versa.
type tee struct {
	a, b slog.Handler
}

// Tee returns a handler that writes to both a and b.
func Tee(a, b slog.Handler) slog.Handler { return &tee{a: a, b: b} }

func (t *tee) Enabled(ctx context.Context, l slog.Level) bool {
	return t.a.Enabled(ctx, l) || t.b.Enabled(ctx, l)
}

func (t *tee) Handle(ctx context.Context, r slog.Record) error {
	var errA, errB error
	if t.a.Enabled(ctx, r.Level) {
		errA = t.a.Handle(ctx, r.Clone())
	}
	if t.b.Enabled(ctx, r.Level) {
		errB = t.b.Handle(ctx, r)
	}
	return errors.Join(errA, errB)
}

func (t *tee) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &tee{a: t.a.WithAttrs(attrs), b: t.b.WithAttrs(attrs)}
}

func (t *tee) WithGroup(name string) slog.Handler {
	return &tee{a: t.a.WithGroup(name), b: t.b.WithGroup(name)}
}
