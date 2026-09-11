package migrate

import (
	"maps"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	agentconfig "github.com/devalexllc/polarbeam/internal/agent/config"
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

// compressAfter extracts every add_compression_policy call's compress_after
// interval (in days) from a migration's text, keyed by hypertable/view name.
func compressAfter(t *testing.T, sql string) map[string]int {
	t.Helper()
	re := regexp.MustCompile(`add_compression_policy\('([a-z_0-9]+)',\s*compress_after\s*=>\s*interval '(\d+) days'`)
	out := map[string]int{}
	for _, m := range re.FindAllStringSubmatch(sql, -1) {
		n, err := strconv.Atoi(m[2])
		if err != nil {
			t.Fatal(err)
		}
		out[m[1]] = n
	}
	return out
}

// startOffsets extracts every add_continuous_aggregate_policy call's
// start_offset (in days) from a migration's text, keyed by view name.
func startOffsets(t *testing.T, sql string) map[string]int {
	t.Helper()
	re := regexp.MustCompile(`add_continuous_aggregate_policy\('([a-z_0-9]+)',\s*start_offset\s*=>\s*interval '(\d+) days'`)
	out := map[string]int{}
	for _, m := range re.FindAllStringSubmatch(sql, -1) {
		n, err := strconv.Atoi(m[2])
		if err != nil {
			t.Fatal(err)
		}
		out[m[1]] = n
	}
	return out
}

// The columnstore policies (0026) sit inside the offset chain 0004/0010/0016
// document: a chunk must never be compressed while a spool replay can still
// land in it, while a refresh policy can still read it as a source, or
// while a refresh policy can still rewrite it as a materialization. The
// migration is immutable once shipped, so pin the arithmetic against the
// agent's shipped spool default and the shipped refresh policies.
func TestColumnstoreOffsetsKeepRefreshAndSpoolInvariants(t *testing.T) {
	read := func(name string) string {
		raw, err := migrations.ReadFile("sql/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	cs := compressAfter(t, read("0026_columnstore.sql"))
	refresh := startOffsets(t, read("0004_policies.sql"))
	maps.Copy(refresh, startOffsets(t, read("0010_health_policies.sql")))
	maps.Copy(refresh, startOffsets(t, read("0016_stage_policies.sql")))

	spoolDays := int(agentconfig.Defaults().Spool.MaxAge / (24 * time.Hour))
	const rawRetentionDays = 14

	raw, ok := cs["probe_results"]
	if !ok {
		t.Fatal("0026 does not add a compression policy on probe_results")
	}
	if raw <= spoolDays {
		t.Errorf("raw compress_after %dd must exceed the agent spool max_age %dd", raw, spoolDays)
	}
	if raw >= rawRetentionDays {
		t.Errorf("raw compress_after %dd must be below raw retention %dd", raw, rawRetentionDays)
	}
	for _, view := range []string{"probe_results_hourly", "probe_results_health_30m", "probe_results_stage_hourly"} {
		if raw < refresh[view] {
			t.Errorf("raw compress_after %dd is inside %s's refresh window (start_offset %dd)", raw, view, refresh[view])
		}
	}
	for view, after := range cs {
		if view == "probe_results" {
			continue
		}
		so, ok := refresh[view]
		if !ok {
			t.Errorf("%s has a compression policy but no shipped refresh policy", view)
			continue
		}
		// A refresh window floors to the bucket, so a daily view's policy
		// can reach one bucket further back than its start_offset.
		if after <= so+1 {
			t.Errorf("%s compress_after %dd must exceed its refresh start_offset %dd plus a bucket of slack", view, after, so)
		}
	}
	if _, ok := cs["probe_results_health_30m"]; ok {
		t.Error("probe_results_health_30m must not be compressed (14d retention on 10-day chunks)")
	}

	// A compressed hypertable requires every unique-index column to be a
	// segmentby or orderby column; the dedupe index is (agent_id, probe_id,
	// time), pinned by 0001.
	sql := read("0026_columnstore.sql")
	rawSettings := regexp.MustCompile(`(?s)ALTER TABLE probe_results SET \((.*?)\);`).FindStringSubmatch(sql)
	if rawSettings == nil {
		t.Fatal("0026 does not ALTER TABLE probe_results")
	}
	covered := map[string]bool{}
	for _, opt := range []string{"segmentby", "orderby"} {
		m := regexp.MustCompile(`timescaledb\.` + opt + `\s*=\s*'([^']*)'`).FindStringSubmatch(rawSettings[1])
		if m == nil {
			t.Fatalf("probe_results columnstore settings lack timescaledb.%s", opt)
		}
		for entry := range strings.SplitSeq(m[1], ",") {
			// An orderby entry is "column [ASC|DESC] [NULLS ...]"; the
			// column is its first word.
			covered[strings.Fields(entry)[0]] = true
		}
	}
	for _, col := range []string{"agent_id", "probe_id", "time"} {
		if !covered[col] {
			t.Errorf("probe_results columnstore settings omit dedupe index column %q (covered: %v)", col, covered)
		}
	}
	if !regexp.MustCompile(`(?s)probe_results_dedupe_uidx\s+ON probe_results \(agent_id, probe_id, time\)`).MatchString(read("0001_init.sql")) {
		t.Error("0001's dedupe index text moved; update the column list above")
	}
}
