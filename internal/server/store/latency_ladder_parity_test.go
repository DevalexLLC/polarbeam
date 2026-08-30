package store

// The latency COALESCE ladder exists in three places that nothing but these
// tests ties together: the Go constants below, the ingest-time grading in
// grpcapi (rowLatencyUS, fenced by its own test against the same fixture),
// and the SQL frozen into the serving hourly cagg. A drift makes
// short-window (raw) and long-window (cagg) charts disagree about the same
// measurement — silently. See testdata/latency-ladder.json.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

type ladderFixture struct {
	Columns []string `json:"columns"`
	Sources []string `json:"sources"`
}

func loadLadder(t *testing.T) ladderFixture {
	t.Helper()
	b, err := os.ReadFile("testdata/latency-ladder.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var f ladderFixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if len(f.Columns) == 0 || len(f.Columns) != len(f.Sources) {
		t.Fatalf("fixture malformed: %d columns, %d sources", len(f.Columns), len(f.Sources))
	}
	return f
}

// sourceCaseRe matches one branch of a latency_source CASE — the only CASE
// shape (here and in the cagg SQL) that tests a *_us column for NOT NULL
// and yields a quoted label.
var sourceCaseRe = regexp.MustCompile(`(\w+_us)\s+IS NOT NULL THEN '(\w*)'`)

// assertLadderCase extracts EVERY branch of the latency_source CASE in text
// and requires the complete ordered (column, source) list to equal the
// fixture — an extra unlisted branch fails, not just a missing one, because
// rows classified into an unlisted source could never be selected by
// chooseLatencySource.
func assertLadderCase(t *testing.T, where, text string, f ladderFixture) {
	t.Helper()
	matches := sourceCaseRe.FindAllStringSubmatch(text, -1)
	if len(matches) != len(f.Columns) {
		t.Fatalf("%s: latency_source CASE has %d branches, fixture has %d", where, len(matches), len(f.Columns))
	}
	for i, m := range matches {
		if m[1] != f.Columns[i] || m[2] != f.Sources[i] {
			t.Errorf("%s: CASE branch %d maps %s -> %q, fixture says %s -> %q",
				where, i, m[1], m[2], f.Columns[i], f.Sources[i])
		}
	}
}

func TestLatencyExprMatchesFixture(t *testing.T) {
	f := loadLadder(t)
	want := "COALESCE(" + strings.Join(f.Columns, ", ") + ")"
	if latencyExpr != want {
		t.Errorf("latencyExpr = %q, fixture says %q", latencyExpr, want)
	}
	if !slices.Equal(latencySourcePriority, f.Sources) {
		t.Errorf("latencySourcePriority = %v, fixture says %v", latencySourcePriority, f.Sources)
	}
	assertLadderCase(t, "latencySourceExpr", latencySourceExpr, f)
}

// TestServingCaggFreezesTheSameLadder pins the fixture to the LATEST
// migration that defines a latency_source cagg — the serving definition.
// Shipped migrations are immutable, so a ladder change means a NEW cagg
// migration: that new file then becomes the latest and must carry the new
// ladder, while 0002 stays frozen with the old one and out of scope here.
// A Go-side reorder without that new cagg fails against the current latest.
func TestServingCaggFreezesTheSameLadder(t *testing.T) {
	f := loadLadder(t)
	files, err := filepath.Glob("../migrate/sql/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob migrations: %v (%d files)", err, len(files))
	}
	sort.Strings(files) // filename order == application order

	var serving string
	var sql string
	for _, path := range files {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if strings.Contains(string(b), "AS latency_source") {
			serving, sql = path, string(b)
		}
	}
	if serving == "" {
		t.Fatal("no migration defines a latency_source cagg")
	}

	want := "COALESCE(" + strings.Join(f.Columns, ", ") + ")"
	if n := strings.Count(sql, want); n < 4 {
		t.Errorf("%s contains the fixture ladder %d times, want the min/max/sum/count/pctl family (>=4)", serving, n)
	}
	// Any COALESCE over ladder columns must be exactly the fixture ladder —
	// a reordered copy hiding elsewhere in the file fails here.
	for _, line := range strings.Split(sql, "\n") {
		if strings.Contains(line, "COALESCE(") && strings.Contains(line, f.Columns[0]) &&
			!strings.Contains(line, want) {
			t.Errorf("%s line has a divergent ladder: %s", serving, strings.TrimSpace(line))
		}
	}
	assertLadderCase(t, serving, sql, f)
}
