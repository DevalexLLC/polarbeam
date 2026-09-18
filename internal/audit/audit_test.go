package audit

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// capture collects records so tests can assert on attribute order and
// levels without parsing text.
type capture struct {
	recs []slog.Record
}

func (c *capture) Enabled(context.Context, slog.Level) bool { return true }
func (c *capture) Handle(_ context.Context, r slog.Record) error {
	c.recs = append(c.recs, r)
	return nil
}
func (c *capture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *capture) WithGroup(string) slog.Handler      { return c }

func keys(r slog.Record) []string {
	var out []string
	r.Attrs(func(a slog.Attr) bool {
		out = append(out, a.Key)
		return true
	})
	return out
}

func TestEmitFixedAttrOrderAndLevel(t *testing.T) {
	c := &capture{}
	l := New(slog.New(c))
	l.Emit(context.Background(), Event{
		ID: EventLogin, Msg: "login", Outcome: Success,
		Actor:  Actor{User: "alice", Source: SourceLocal, Session: "s-1"},
		Remote: "203.0.113.9",
		Attrs:  []slog.Attr{slog.String("route", "POST /x")},
	})
	if len(c.recs) != 1 {
		t.Fatalf("records = %d, want 1", len(c.recs))
	}
	r := c.recs[0]
	if r.Level != slog.LevelInfo {
		t.Errorf("level = %v, want INFO for success", r.Level)
	}
	got := strings.Join(keys(r), ",")
	want := "event,outcome,user,user_source,session,remote,route"
	if got != want {
		t.Errorf("attr order = %s, want %s", got, want)
	}
	if !IsAudit(r) || EventID(r) != EventLogin {
		t.Errorf("IsAudit/EventID did not recognise the record")
	}

	l.Emit(context.Background(), Event{ID: EventLogin, Outcome: Failure})
	l.Emit(context.Background(), Event{ID: EventLogin, Outcome: Denied})
	for i, want := range []slog.Level{slog.LevelWarn, slog.LevelWarn} {
		if got := c.recs[1+i].Level; got != want {
			t.Errorf("record %d level = %v, want %v", 1+i, got, want)
		}
	}
	// Empty actor fields are omitted, not written as "".
	if got := strings.Join(keys(c.recs[1]), ","); got != "event,outcome" {
		t.Errorf("minimal record attrs = %s, want event,outcome", got)
	}
}

func TestEmitAtOverridesRecordTime(t *testing.T) {
	c := &capture{}
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	New(slog.New(c)).Emit(context.Background(), Event{ID: EventSessionEnded, Outcome: Success, At: at})
	if !c.recs[0].Time.Equal(at) {
		t.Errorf("record time = %v, want %v", c.recs[0].Time, at)
	}
}

func TestEmitRefusesSecretsAndUnknownIDs(t *testing.T) {
	l := New(slog.New(&capture{}))
	for _, key := range []string{"password", "client_secret", "csrf_token", "ca_pem", "Authorization", "cookie"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("key %q was accepted", key)
				}
			}()
			l.Emit(context.Background(), Event{ID: EventLogin, Outcome: Success, Attrs: []slog.Attr{slog.String(key, "x")}})
		}()
	}
	// Keys that merely contain a forbidden word as a substring are fine.
	for _, key := range []string{"tokens_deleted", "secretary", "pemberton"} {
		if forbidden(key) {
			t.Errorf("key %q wrongly refused", key)
		}
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("unknown event id was accepted")
			}
		}()
		l.Emit(context.Background(), Event{ID: "nope.nope", Outcome: Success})
	}()
}

func TestNilLoggerUsesDefault(t *testing.T) {
	c := &capture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(c))
	t.Cleanup(func() { slog.SetDefault(prev) })
	var l *Logger
	l.Emit(context.Background(), Event{ID: EventServerStart, Outcome: Success})
	New(nil).Emit(context.Background(), Event{ID: EventServerStop, Outcome: Success})
	if len(c.recs) != 2 {
		t.Fatalf("records = %d, want 2 through the default logger", len(c.recs))
	}
}

func TestExtras(t *testing.T) {
	ctx, ex := WithExtras(context.Background())
	Add(ctx, slog.String("target", "t1"))
	Deny(ctx, "out_of_scope")
	attrs, denied, failed, suppressed := ex.Take()
	if len(attrs) != 2 || attrs[0].Key != "target" || attrs[1].Key != "reason" {
		t.Errorf("attrs = %v", attrs)
	}
	if !denied || failed || suppressed {
		t.Errorf("denied=%v failed=%v suppressed=%v", denied, failed, suppressed)
	}
	Suppress(ctx)
	Fail(ctx, "f")
	if _, _, f, s := ex.Take(); !s || !f {
		t.Error("Suppress/Fail not recorded")
	}
	// No Extras in the context: silent no-ops.
	Add(context.Background(), slog.String("x", "y"))
	Deny(context.Background(), "r")
	Fail(context.Background(), "r")
	Suppress(context.Background())
	var nilEx *Extras
	if a, d, f, s := nilEx.Take(); a != nil || d || f || s {
		t.Error("nil Extras must read as empty")
	}
}
