package mtuwatch

// Statement-count contract on the ingest hot path, mirroring pathwatch's
// (and originally outage's): however many series and changes a push
// carries, Apply issues a fixed set of statements — never one per series
// or per event.

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

// scriptedDB satisfies DB, records every statement, and answers the lock
// query from lockRows and the event insert's RETURNING from the queued
// probe IDs (args[2] of the insert). Everything else gets zero rows.
type scriptedDB struct {
	execs    []string
	queries  []string
	lockRows [][]any
}

func (d *scriptedDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	d.execs = append(d.execs, sql)
	// The bulk upsert now errors unless RowsAffected matches its input
	// size; echo the queued row count (args[1] is the probe ID array).
	n := 0
	if ids, ok := args[1].([]uuid.UUID); ok {
		n = len(ids)
	}
	return pgconn.NewCommandTag(fmt.Sprintf("INSERT 0 %d", n)), nil
}

func (d *scriptedDB) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	d.queries = append(d.queries, sql)
	switch {
	case strings.Contains(sql, "FOR UPDATE"):
		return &valueRows{rows: d.lockRows}, nil
	case strings.Contains(sql, "path_mtu_events"):
		probeIDs := args[2].([]uuid.UUID)
		rows := make([][]any, len(probeIDs))
		for i, id := range probeIDs {
			rows[i] = []any{uuid.New(), id}
		}
		return &valueRows{rows: rows}, nil
	default:
		return &valueRows{}, nil
	}
}

func (d *scriptedDB) QueryRow(context.Context, string, ...any) pgx.Row {
	panic("unexpected QueryRow: the bulk Apply never uses it")
}

// valueRows plays back preset rows; zero value = empty result set.
type valueRows struct {
	rows [][]any
	i    int
}

func (r *valueRows) Next() bool { r.i++; return r.i <= len(r.rows) }
func (r *valueRows) Scan(dest ...any) error {
	row := r.rows[r.i-1]
	for j, d := range dest {
		switch p := d.(type) {
		case *uuid.UUID:
			*p = row[j].(uuid.UUID)
		case *time.Time:
			*p = row[j].(time.Time)
		case *int32:
			*p = row[j].(int32)
		case *bool:
			*p = row[j].(bool)
		default:
			return fmt.Errorf("valueRows: unsupported dest %T", d)
		}
	}
	return nil
}
func (r *valueRows) Close()                                       {}
func (r *valueRows) Err() error                                   { return nil }
func (r *valueRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *valueRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *valueRows) Values() ([]any, error)                       { return nil, nil }
func (r *valueRows) RawValues() [][]byte                          { return nil }
func (r *valueRows) Conn() *pgx.Conn                              { return nil }

func contractRuns(n int, mtu int32) []Run {
	runs := make([]Run, n)
	for i := range n {
		r := run(int64(100+i), mtu, false, true)
		r.ProbeID = uuid.MustParse(fmt.Sprintf("33333333-3333-3333-3333-3333333333%02d", i))
		runs[i] = r
	}
	return runs
}

func TestApplyIssuesBulkStatements(t *testing.T) {
	// Fresh series, no changes: exactly the seed insert and the lock —
	// both queries — plus ONE bulk current upsert.
	db := &scriptedDB{}
	if _, err := Apply(context.Background(), db, uuid.NameSpaceOID, contractRuns(3, 1500)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(db.queries) != 2 ||
		!strings.Contains(db.queries[0], "ON CONFLICT (agent_id, probe_id) DO NOTHING") ||
		!strings.Contains(db.queries[0], "RETURNING probe_id") ||
		!strings.Contains(db.queries[1], "FOR UPDATE") {
		t.Errorf("queries = %d %q, want exactly seed insert then FOR UPDATE lock", len(db.queries), db.queries)
	}
	if len(db.execs) != 1 {
		t.Fatalf("execs = %d, want exactly the bulk upsert: %q", len(db.execs), db.execs)
	}
	bulk := db.execs[0]
	for _, want := range []string{"unnest", "ON CONFLICT (agent_id, probe_id) DO UPDATE",
		"updated_at < EXCLUDED.updated_at"} {
		if !strings.Contains(bulk, want) {
			t.Errorf("bulk upsert misses %q: %q", want, bulk)
		}
	}
}

func TestApplyIssuesOneBulkEventInsert(t *testing.T) {
	// Every series exists with an older, different MTU: N changes, and
	// still ONE event insert (a query, for RETURNING) — never one per event.
	runs := contractRuns(3, 1400)
	db := &scriptedDB{}
	for _, r := range runs {
		db.lockRows = append(db.lockRows, []any{
			r.ProbeID, time.Unix(50, 0).UTC(), int32(1500), false,
		})
	}
	changes, err := Apply(context.Background(), db, uuid.NameSpaceOID, runs)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(changes) != 3 {
		t.Fatalf("changes = %d, want 3", len(changes))
	}
	if len(db.queries) != 3 ||
		!strings.Contains(db.queries[2], "path_mtu_events") ||
		!strings.Contains(db.queries[2], "unnest") ||
		!strings.Contains(db.queries[2], "RETURNING id, probe_id") {
		t.Errorf("queries = %d, want seed+lock+ONE bulk event insert: %q", len(db.queries), db.queries)
	}
	if len(db.execs) != 1 {
		t.Errorf("execs = %d, want exactly the bulk upsert: %q", len(db.execs), db.execs)
	}
	for i, c := range changes {
		if c.ProbeID != runs[i].ProbeID || c.OldMTU != 1500 || c.NewMTU != 1400 {
			t.Errorf("changes[%d] = %+v, want probe %s 1500->1400", i, c, runs[i].ProbeID)
		}
	}
}

func TestApplyDropsSentinelTimeRuns(t *testing.T) {
	// A run stamped at Go's zero time would collide with the seed
	// sentinel: the upsert guard would skip it and commit the placeholder
	// as real state. It must be dropped before grouping — a push of only
	// such runs touches the database not at all, and a valid run
	// alongside one proceeds normally.
	zero := run(100, 1500, false, true)
	zero.Time = time.Time{}
	db := &scriptedDB{}
	if _, err := Apply(context.Background(), db, uuid.NameSpaceOID, []Run{zero}); err != nil {
		t.Fatalf("Apply(sentinel only): %v", err)
	}
	if len(db.queries) != 0 || len(db.execs) != 0 {
		t.Errorf("sentinel-only push issued statements: %q %q", db.queries, db.execs)
	}
	valid := run(200, 1400, false, true)
	if _, err := Apply(context.Background(), db, uuid.NameSpaceOID, []Run{zero, valid}); err != nil {
		t.Fatalf("Apply(sentinel + valid): %v", err)
	}
	if len(db.execs) != 1 {
		t.Errorf("valid run alongside a sentinel one did not reach the upsert: %q", db.execs)
	}
}
