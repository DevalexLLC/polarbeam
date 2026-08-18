package migrate

import (
	"strings"
	"testing"
)

// Notx migrations run as a single autocommit Exec; a second statement in the
// same file would resurrect the implicit transaction TimescaleDB rejects for
// continuous aggregate creation, and non-idempotent DDL would break the
// re-run after a crash between the DDL and its schema_migrations record.
// This test pins both invariants for every embedded .notx.sql file.
func TestNotxMigrationsAreSingleIdempotentStatements(t *testing.T) {
	names, err := embeddedNames()
	if err != nil {
		t.Fatal(err)
	}
	sawNotx := false
	for _, name := range names {
		if !strings.HasSuffix(name, ".notx.sql") {
			continue
		}
		sawNotx = true
		raw, err := migrations.ReadFile("sql/" + name)
		if err != nil {
			t.Fatal(err)
		}
		sql := stripLineComments(string(raw))
		if got := strings.Count(sql, ";"); got != 1 {
			t.Errorf("%s: want exactly 1 statement (1 semicolon outside comments), got %d", name, got)
		}
		stmt := strings.TrimSpace(sql)
		if !strings.HasSuffix(stmt, ";") {
			t.Errorf("%s: content after the final semicolon", name)
		}
		// Idempotency is per statement shape: CREATE needs IF NOT EXISTS;
		// refresh recomputes the same buckets, so a re-run converges. Any
		// other shape must prove itself here before it ships.
		switch {
		case strings.HasPrefix(stmt, "CREATE"):
			if !strings.Contains(stmt, "IF NOT EXISTS") {
				t.Errorf("%s: CREATE in a notx file must use IF NOT EXISTS", name)
			}
		case strings.HasPrefix(stmt, "CALL refresh_continuous_aggregate"):
			// naturally idempotent
		default:
			t.Errorf("%s: unrecognized notx statement shape — prove it is idempotent and extend this test", name)
		}
	}
	if !sawNotx {
		t.Skip("no .notx.sql migrations embedded")
	}
}

// stripLineComments removes -- comments. The notx files use neither string
// literals containing "--" nor dollar quoting, so line-level stripping is
// sufficient; keep it that way.
func stripLineComments(sql string) string {
	var b strings.Builder
	for line := range strings.SplitSeq(sql, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

func TestLatencyAggregatesKeepTimingFamiliesHonest(t *testing.T) {
	hourly, err := migrations.ReadFile("sql/0002_hourly_cagg.notx.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(hourly)
	for _, want := range []string{
		"latency_source",
		"CASE WHEN status = 1 THEN COALESCE",
		"FILTER (WHERE status = 1)",
		"GROUP BY bucket, agent_id, target_id, probe_type, latency_source",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("hourly aggregate missing %q", want)
		}
	}

	daily, err := migrations.ReadFile("sql/0003_daily_cagg.notx.sql")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(daily), "probe_type, latency_source") {
		t.Error("daily aggregate does not preserve the hourly latency source partition")
	}
}

// The health strips' store queries filter and fold on exactly these group
// keys, freeze status = 1 as the only success, and rely on the live tail of
// materialized_only = false for the current half hour. The cagg definition
// is immutable once shipped, so pin the contract.
func TestHealthAggregateKeepsStripSemantics(t *testing.T) {
	health, err := migrations.ReadFile("sql/0009_health_cagg.notx.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(health)
	for _, want := range []string{
		"GROUP BY bucket, agent_id, probe_id, probe_type",
		"count(*) FILTER (WHERE status = 1)",
		"timescaledb.materialized_only = false",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("health aggregate missing %q", want)
		}
	}
}

// The target detail page's stage queries filter and fold on exactly these
// group keys, compute averages as sum/count (so the daily rollup must stay
// sums-only), freeze status = 1 as the only success, and rely on the live
// tail of materialized_only = false. The cagg definitions are immutable once
// shipped, so pin the contract.
func TestStageAggregatesKeepStageSemantics(t *testing.T) {
	hourly, err := migrations.ReadFile("sql/0014_stage_hourly_cagg.notx.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(hourly)
	wants := []string{
		"GROUP BY bucket, agent_id, target_id, probe_type",
		"timescaledb.materialized_only = false",
	}
	for _, stage := range []string{"dns", "tcp", "tls", "ttfb", "total"} {
		wants = append(wants,
			stage+"_sum_us",
			stage+"_count",
		)
	}
	// Every timing measure aggregates successful probes only.
	wants = append(wants, "FILTER (WHERE status = 1)")
	for _, want := range wants {
		if !strings.Contains(sql, want) {
			t.Errorf("stage hourly aggregate missing %q", want)
		}
	}
	if strings.Contains(stripLineComments(sql), "avg(") {
		t.Error("stage hourly aggregate must store sums/counts, never avg() — the daily rollup would be wrong")
	}

	daily, err := migrations.ReadFile("sql/0015_stage_daily_cagg.notx.sql")
	if err != nil {
		t.Fatal(err)
	}
	dsql := string(daily)
	if !strings.Contains(dsql, "FROM probe_results_stage_hourly") {
		t.Error("stage daily aggregate must roll up the hourly stage cagg")
	}
	if !strings.Contains(dsql, "agent_id, target_id, probe_type") {
		t.Error("stage daily aggregate does not preserve the hourly group keys")
	}
	if strings.Contains(stripLineComments(dsql), "avg(") {
		t.Error("stage daily aggregate must re-sum, never avg()")
	}
}
