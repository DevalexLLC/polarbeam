package outage

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func result(secs int64, ok bool) Result {
	code := int16(1)
	if !ok {
		code = 2 // TIMEOUT
	}
	return Result{
		ProbeID:    uuid.NameSpaceDNS,
		TargetID:   uuid.NameSpaceURL,
		ProbeType:  1,
		Time:       time.Unix(secs, 0).UTC(),
		OK:         ok,
		StatusCode: code,
	}
}

// foldEvent is one open or close fold observed, with the kind it applied to
// (for closes, the kind that was open) and the event timestamp.
type foldEvent struct {
	act  action
	kind string
	at   time.Time
}

// fold runs a sequence through step, executing ops the way Apply does
// (open sets OpenEventID+OpenKind, close clears them), and returns the
// event transitions taken.
func fold(st State, results []Result) (State, []foldEvent) {
	var events []foldEvent
	n := 0
	for _, r := range results {
		var ops []op
		st, ops = step(st, r)
		for _, o := range ops {
			switch o.act {
			case actionOpen:
				n++
				id := uuid.MustParse(fmt.Sprintf("11111111-1111-1111-1111-1111111111%02d", n))
				st.OpenEventID = &id
				st.OpenKind = o.kind
				events = append(events, foldEvent{act: actionOpen, kind: o.kind, at: o.at})
			case actionClose:
				events = append(events, foldEvent{act: actionClose, kind: st.OpenKind, at: o.at})
				st.OpenEventID = nil
				st.OpenKind = ""
			}
		}
	}
	return st, events
}

func seq(pattern string) []Result {
	// pattern: 'o' = clean OK, 'f' = failure, 'd' = OK breaching the
	// critical thresholds; timestamps ascend one second apart from t=100.
	rs := make([]Result, len(pattern))
	for i, c := range pattern {
		r := result(int64(100+i), c != 'f')
		if c == 'd' {
			r.Degraded = true
			r.DegradedDetail = "latency at or above critical threshold (40ms)"
		}
		rs[i] = r
	}
	return rs
}

func count(events []foldEvent, act action, kind string) int {
	n := 0
	for _, e := range events {
		if e.act == act && e.kind == kind {
			n++
		}
	}
	return n
}

func TestOpensAtThirdConsecutiveFailure(t *testing.T) {
	st, events := fold(State{}, seq("offfff"))
	if got := count(events, actionOpen, KindProbeFailing); got != 1 {
		t.Fatalf("failing opens = %d, want exactly 1: %+v", got, events)
	}
	// opened_at is the FIRST failure of the streak (t=101).
	if !events[0].at.Equal(time.Unix(101, 0).UTC()) {
		t.Errorf("opened_at = %v, want t=101", events[0].at)
	}
	if !st.FirstFailAt.Equal(time.Unix(101, 0).UTC()) {
		t.Errorf("first_fail_at = %v, want t=101", st.FirstFailAt)
	}
	if st.OpenEventID == nil || st.OpenKind != KindProbeFailing {
		t.Errorf("open event must be recorded in state: %+v", st)
	}
}

func TestClosesAtThirdConsecutiveSuccess(t *testing.T) {
	st, events := fold(State{}, seq("fffooo"))
	if count(events, actionOpen, KindProbeFailing) != 1 || count(events, actionClose, KindProbeFailing) != 1 {
		t.Fatalf("events = %+v, want one failing open and one failing close", events)
	}
	// closed_at is the FIRST success of the closing streak (t=103).
	if !events[1].at.Equal(time.Unix(103, 0).UTC()) {
		t.Errorf("closed_at = %v, want t=103", events[1].at)
	}
	if st.OpenEventID != nil || st.OpenKind != "" {
		t.Errorf("close must clear the open event: %+v", st)
	}
}

func TestPartialRecoveryDoesNotClose(t *testing.T) {
	// Two OKs inside an open outage are not enough; a failure resets the
	// success streak without opening a second event.
	_, events := fold(State{}, seq("fffoofoo"))
	if count(events, actionOpen, KindProbeFailing) != 1 {
		t.Errorf("partial recovery must not reopen: %+v", events)
	}
	if count(events, actionClose, KindProbeFailing) != 0 {
		t.Errorf("partial recovery must not close: %+v", events)
	}
}

func TestFlappingNeverReachesThreshold(t *testing.T) {
	_, events := fold(State{}, seq("ffoffoffo"))
	if len(events) != 0 {
		t.Errorf("flapping below threshold must never open: %+v", events)
	}
}

func TestOutOfOrderResultsIgnored(t *testing.T) {
	st, _ := fold(State{}, seq("fff")) // open, last_time = 102
	// A replayed older OK must not disturb the streak.
	st2, ops := step(st, result(50, true))
	if len(ops) != 0 || st2.ConsecFails != st.ConsecFails || st2.ConsecOKs != 0 {
		t.Errorf("older result changed state: %+v ops %+v", st2, ops)
	}
	// Same timestamp is also ignored (dedupe boundary).
	st3, ops := step(st, result(102, true))
	if len(ops) != 0 || st3.ConsecFails != st.ConsecFails {
		t.Errorf("same-time result changed state: %+v ops %+v", st3, ops)
	}
}

func TestReplayedHistoryOpensAndCloses(t *testing.T) {
	// A spool replay carrying a whole outage and its recovery in one batch
	// must produce exactly one open and one close.
	_, events := fold(State{}, seq("ooofffffooo"))
	if count(events, actionOpen, KindProbeFailing) != 1 || count(events, actionClose, KindProbeFailing) != 1 {
		t.Errorf("replayed outage: events = %+v", events)
	}
}

func TestDriftHealsWithOpenPastThreshold(t *testing.T) {
	// State says no open event but the streak is already past threshold
	// (crash between event insert and state upsert): the next failure must
	// still try to open.
	st := State{ConsecFails: 5, LastTime: time.Unix(100, 0).UTC()}
	_, ops := step(st, result(101, false))
	if len(ops) != 1 || ops[0].act != actionOpen || ops[0].kind != KindProbeFailing {
		t.Errorf("drifted state must re-attempt open, got %+v", ops)
	}
}

func TestDegradedOpensAtThirdConsecutiveBreach(t *testing.T) {
	st, events := fold(State{}, seq("oddd"))
	if count(events, actionOpen, KindProbeDegraded) != 1 || len(events) != 1 {
		t.Fatalf("events = %+v, want exactly one degraded open", events)
	}
	// opened_at is the FIRST breaching success of the streak (t=101).
	if !events[0].at.Equal(time.Unix(101, 0).UTC()) {
		t.Errorf("opened_at = %v, want t=101", events[0].at)
	}
	if st.OpenKind != KindProbeDegraded {
		t.Errorf("open kind = %q, want probe_degraded", st.OpenKind)
	}
}

func TestDegradedClosesAtThirdConsecutiveClean(t *testing.T) {
	st, events := fold(State{}, seq("dddooo"))
	if count(events, actionOpen, KindProbeDegraded) != 1 || count(events, actionClose, KindProbeDegraded) != 1 {
		t.Fatalf("events = %+v, want one degraded open and one degraded close", events)
	}
	// closed_at is the FIRST clean success of the closing streak (t=103).
	if !events[1].at.Equal(time.Unix(103, 0).UTC()) {
		t.Errorf("closed_at = %v, want t=103", events[1].at)
	}
	if st.OpenEventID != nil || st.OpenKind != "" {
		t.Errorf("close must clear the open event: %+v", st)
	}
}

func TestEscalationClosesDegradedAndOpensFailing(t *testing.T) {
	// A degraded link going hard-down swaps the open event in one step,
	// with contiguous timestamps (degraded closes where failing opens).
	_, events := fold(State{}, seq("dddfff"))
	want := []foldEvent{
		{act: actionOpen, kind: KindProbeDegraded, at: time.Unix(100, 0).UTC()},
		{act: actionClose, kind: KindProbeDegraded, at: time.Unix(103, 0).UTC()},
		{act: actionOpen, kind: KindProbeFailing, at: time.Unix(103, 0).UTC()},
	}
	if len(events) != len(want) {
		t.Fatalf("events = %+v, want %+v", events, want)
	}
	for i := range want {
		if events[i].act != want[i].act || events[i].kind != want[i].kind || !events[i].at.Equal(want[i].at) {
			t.Errorf("event %d = %+v, want %+v", i, events[i], want[i])
		}
	}
}

func TestDeEscalationOpensDegradedWhenClosingSuccessesBreach(t *testing.T) {
	// Three breaching successes close the outage AND open degraded in the
	// same step — deferring the open would lose a real 3-sample breach
	// window if the next result is clean.
	_, events := fold(State{}, seq("fffddd"))
	want := []foldEvent{
		{act: actionOpen, kind: KindProbeFailing, at: time.Unix(100, 0).UTC()},
		{act: actionClose, kind: KindProbeFailing, at: time.Unix(103, 0).UTC()},
		{act: actionOpen, kind: KindProbeDegraded, at: time.Unix(103, 0).UTC()},
	}
	if len(events) != len(want) {
		t.Fatalf("events = %+v, want %+v", events, want)
	}
	for i := range want {
		if events[i].act != want[i].act || events[i].kind != want[i].kind || !events[i].at.Equal(want[i].at) {
			t.Errorf("event %d = %+v, want %+v", i, events[i], want[i])
		}
	}
}

func TestFailureResetsDegradedStreak(t *testing.T) {
	_, events := fold(State{}, seq("ddfdd"))
	if len(events) != 0 {
		t.Errorf("interrupted breach streaks must not open: %+v", events)
	}
}

func TestCleanSuccessResetsDegradedStreak(t *testing.T) {
	_, events := fold(State{}, seq("ddodd"))
	if len(events) != 0 {
		t.Errorf("interrupted breach streaks must not open: %+v", events)
	}
}

func TestMixedTailKeepsDegradedOpen(t *testing.T) {
	// Two clean successes inside an open degraded event are not enough,
	// and a breaching success resets the clean streak.
	st, events := fold(State{}, seq("dddood"))
	if len(events) != 1 || count(events, actionOpen, KindProbeDegraded) != 1 {
		t.Fatalf("events = %+v, want only the degraded open", events)
	}
	if st.OpenKind != KindProbeDegraded {
		t.Errorf("degraded event must stay open: %+v", st)
	}
}

func TestDegradedDriftHealsPastThreshold(t *testing.T) {
	st := State{ConsecDegraded: 5, ConsecOKs: 5, LastTime: time.Unix(100, 0).UTC()}
	r := result(101, true)
	r.Degraded = true
	_, ops := step(st, r)
	if len(ops) != 1 || ops[0].act != actionOpen || ops[0].kind != KindProbeDegraded {
		t.Errorf("drifted state must re-attempt degraded open, got %+v", ops)
	}
}

// recordingDB satisfies DB, records every statement, and answers queries
// with zero rows (every series folds from a zero State).
type recordingDB struct {
	execs   []string
	queries []string
}

func (d *recordingDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	d.execs = append(d.execs, sql)
	return pgconn.CommandTag{}, nil
}

func (d *recordingDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	d.queries = append(d.queries, sql)
	return emptyRows{}, nil
}

func (d *recordingDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	panic("unexpected QueryRow: no transitions in this test")
}

type emptyRows struct{}

func (emptyRows) Close()                                       {}
func (emptyRows) Err() error                                   { return nil }
func (emptyRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (emptyRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (emptyRows) Next() bool                                   { return false }
func (emptyRows) Scan(dest ...any) error                       { return nil }
func (emptyRows) Values() ([]any, error)                       { return nil, nil }
func (emptyRows) RawValues() [][]byte                          { return nil }
func (emptyRows) Conn() *pgx.Conn                              { return nil }

func TestApplyIssuesOneBulkStateUpsert(t *testing.T) {
	// The round-trip contract on the ingest hot path: however many series
	// a push carries (without transitions), Apply issues exactly one seed
	// insert, one locking select, and ONE bulk state upsert — never a
	// statement per series.
	db := &recordingDB{}
	var results []Result
	for i := range 3 {
		r := result(int64(100+i), true)
		r.ProbeID = uuid.MustParse("22222222-2222-2222-2222-22222222222" + string(rune('0'+i)))
		results = append(results, r)
	}
	if _, err := Apply(context.Background(), db, uuid.NameSpaceOID, results); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// FOR UPDATE OF ss, not plain FOR UPDATE: the OpenKind join must not
	// lock outage_events (and plain FOR UPDATE errors on an outer join).
	// ORDER BY ss.probe_id must precede it: lock acquisition order across
	// concurrent transactions comes from this clause, not the Go-side sort.
	if len(db.queries) != 1 {
		t.Fatalf("queries = %d %q, want exactly the lock select", len(db.queries), db.queries)
	}
	lock := db.queries[0]
	order, upd := strings.Index(lock, "ORDER BY ss.probe_id"), strings.Index(lock, "FOR UPDATE OF ss")
	if order < 0 || upd < 0 || order > upd {
		t.Errorf("lock select misses ORDER BY ss.probe_id before FOR UPDATE OF ss: %q", lock)
	}
	if len(db.execs) != 2 {
		t.Fatalf("execs = %d, want exactly 2 (seed insert + bulk upsert): %q", len(db.execs), db.execs)
	}
	if !strings.Contains(db.execs[0], "DO NOTHING") {
		t.Errorf("first exec is not the seed insert: %q", db.execs[0])
	}
	bulk := db.execs[1]
	if !strings.Contains(bulk, "unnest") || !strings.Contains(bulk, "ON CONFLICT (agent_id, probe_id) DO UPDATE") {
		t.Errorf("second exec is not the bulk upsert: %q", bulk)
	}
	for _, col := range []string{"consec_degraded", "consec_clean", "first_degraded_at", "first_clean_at"} {
		if !strings.Contains(bulk, col) {
			t.Errorf("bulk upsert misses %s: %q", col, bulk)
		}
	}
}
