package migrate_test

// Upgrade-path tests for 0017_networks.sql: a database populated with
// pre-networks rows must backfill everything onto the seeded default
// network, and — the point of the backfill — expansion over it must produce
// exactly the probe specs, enabled IDs, and expected pairs the old
// network-blind code produced, so config hashes never move on upgrade.

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/devalexllc/polarbeam/internal/server/dbtest"
	"github.com/devalexllc/polarbeam/internal/server/meshexpand"
	"github.com/devalexllc/polarbeam/internal/server/migrate"
	"github.com/devalexllc/polarbeam/internal/server/probeid"
	"github.com/devalexllc/polarbeam/internal/server/store"
)

func TestNetworksMigrationUpgrade(t *testing.T) {
	url := dbtest.Empty(t)
	ctx, conn := connect(t, url)

	if err := migrate.ApplyThrough(ctx, conn, "0016_stage_policies.sql"); err != nil {
		t.Fatalf("apply through 0016: %v", err)
	}

	// Old-shape seed: two staffed sites, a mesh over both with one template,
	// a direct probe against an external target, and an unused join token.
	siteA, siteB := uuid.New(), uuid.New()
	agentA, agentB := uuid.New(), uuid.New()
	targetA, targetB, extern := uuid.New(), uuid.New(), uuid.New()
	meshID, tmplID, directID, tokenID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	seed := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO sites (id, name) VALUES ($1, 'site-a'), ($2, 'site-b')`, []any{siteA, siteB}},
		{`INSERT INTO agents (id, site_id, hostname) VALUES ($1, $3, 'a1'), ($2, $4, 'b1')`,
			[]any{agentA, agentB, siteA, siteB}},
		{`INSERT INTO targets (id, kind, name, agent_id) VALUES ($1, 'agent', 'agent:a1', $3), ($2, 'agent', 'agent:b1', $4)`,
			[]any{targetA, targetB, agentA, agentB}},
		{`INSERT INTO targets (id, kind, name, address, port) VALUES ($1, 'external', 'svc', '203.0.113.7', 443)`,
			[]any{extern}},
		{`INSERT INTO mesh_groups (id, name) VALUES ($1, 'm1')`, []any{meshID}},
		{`INSERT INTO mesh_members (mesh_id, site_id) VALUES ($1, $2), ($1, $3)`, []any{meshID, siteA, siteB}},
		{`INSERT INTO probe_configs (id, mesh_id, probe_type, interval_ms, timeout_ms) VALUES ($1, $2, 1, 60000, 5000)`,
			[]any{tmplID, meshID}},
		{`INSERT INTO probe_configs (id, site_id, target_id, probe_type, interval_ms, timeout_ms) VALUES ($1, $2, $3, 1, 60000, 5000)`,
			[]any{directID, siteA, extern}},
		{`INSERT INTO join_tokens (id, secret_hash, site_id, expires_at) VALUES ($1, '\x00'::bytea, $2, now() + interval '1 hour')`,
			[]any{tokenID, siteA}},
	}
	for _, q := range seed {
		if _, err := conn.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("seed %q: %v", q.sql, err)
		}
	}

	if err := migrate.Apply(ctx, conn); err != nil {
		t.Fatalf("apply remaining migrations: %v", err)
	}

	// Backfill: exactly the seeded default network, and every pre-existing
	// row on it — except mesh template rows, which must stay NULL.
	var defaultNet uuid.UUID
	if err := conn.QueryRow(ctx,
		`SELECT id FROM networks WHERE name = 'default'`).Scan(&defaultNet); err != nil {
		t.Fatalf("default network: %v", err)
	}
	var networks int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM networks`).Scan(&networks); err != nil {
		t.Fatalf("count networks: %v", err)
	}
	if networks != 1 {
		t.Errorf("networks rows = %d, want just the seeded default", networks)
	}
	for _, check := range []struct {
		what, sql string
	}{
		{"agents", `SELECT count(*) FROM agents WHERE network_id <> $1`},
		{"join_tokens", `SELECT count(*) FROM join_tokens WHERE network_id <> $1`},
		{"mesh_groups", `SELECT count(*) FROM mesh_groups WHERE network_id <> $1`},
		{"direct probe_configs", `SELECT count(*) FROM probe_configs WHERE mesh_id IS NULL AND (network_id IS NULL OR network_id <> $1)`},
	} {
		var n int
		if err := conn.QueryRow(ctx, check.sql, defaultNet).Scan(&n); err != nil {
			t.Fatalf("check %s: %v", check.what, err)
		}
		if n != 0 {
			t.Errorf("%d %s rows not backfilled to default", n, check.what)
		}
	}
	var meshRowsWithNet int
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM probe_configs WHERE mesh_id IS NOT NULL AND network_id IS NOT NULL`).Scan(&meshRowsWithNet); err != nil {
		t.Fatalf("check mesh rows: %v", err)
	}
	if meshRowsWithNet != 0 {
		t.Errorf("%d mesh template rows carry a network_id, want NULL (inherited from the mesh)", meshRowsWithNet)
	}

	// No-op upgrade: post-migration expansion equals the pre-networks
	// semantics, computed independently here. Spec-set identity plus the
	// untouched hash function (pinned by meshexpand's own tests) is what
	// keeps config_hash from moving on upgrade.
	s, err := store.Connect(ctx, url, 10*time.Second, 0)
	if err != nil {
		t.Fatalf("store.Connect: %v", err)
	}
	defer s.Close()

	wantByAgent := map[uuid.UUID]map[string]bool{
		agentA: {
			directID.String(): true,
			probeid.MeshProbeID(tmplID, siteA, targetB).String(): true,
		},
		agentB: {
			probeid.MeshProbeID(tmplID, siteB, targetA).String(): true,
		},
	}
	for agentID, want := range wantByAgent {
		in, err := s.LoadAgentConfigInputs(ctx, agentID)
		if err != nil {
			t.Fatalf("LoadAgentConfigInputs %s: %v", agentID, err)
		}
		snap, err := meshexpand.BuildSnapshot(in)
		if err != nil {
			t.Fatalf("BuildSnapshot %s: %v", agentID, err)
		}
		got := make(map[string]bool, len(snap.Probes))
		for _, p := range snap.Probes {
			got[p.ProbeId] = true
		}
		if len(got) != len(want) {
			t.Errorf("agent %s: %d specs %v, want %d %v", agentID, len(got), got, len(want), want)
			continue
		}
		for id := range want {
			if !got[id] {
				t.Errorf("agent %s: missing spec %s", agentID, id)
			}
		}
	}

	enabled, err := s.EnabledProbeIDs(ctx)
	if err != nil {
		t.Fatalf("EnabledProbeIDs: %v", err)
	}
	wantEnabled := map[uuid.UUID]bool{
		directID: true,
		probeid.MeshProbeID(tmplID, siteA, targetB): true,
		probeid.MeshProbeID(tmplID, siteB, targetA): true,
	}
	if len(enabled) != len(wantEnabled) {
		t.Errorf("EnabledProbeIDs = %v, want %d ids", enabled, len(wantEnabled))
	}
	for _, id := range enabled {
		if !wantEnabled[id] {
			t.Errorf("EnabledProbeIDs contains unexpected %s", id)
		}
	}

	pairs, err := s.ExpectedPairs(ctx, nil)
	if err != nil {
		t.Fatalf("ExpectedPairs: %v", err)
	}
	gotPairs := make(map[string]bool, len(pairs))
	for _, p := range pairs {
		gotPairs[p.Src+">"+p.Dst] = true
	}
	if len(gotPairs) != 2 || !gotPairs["site-a>site-b"] || !gotPairs["site-b>site-a"] {
		t.Errorf("ExpectedPairs = %v, want both directions of (site-a, site-b)", gotPairs)
	}
}

// TestNetworksConstraints pins the schema's fail-loud shape: no defaults
// anywhere, so a writer that forgets to assign a network errors instead of
// silently landing on default.
func TestNetworksConstraints(t *testing.T) {
	ctx, conn := connect(t, dbtest.Migrated(t))

	var siteID, defaultNet uuid.UUID
	if err := conn.QueryRow(ctx,
		`INSERT INTO sites (name) VALUES ('site-a') RETURNING id`).Scan(&siteID); err != nil {
		t.Fatalf("insert site: %v", err)
	}
	if err := conn.QueryRow(ctx,
		`SELECT id FROM networks WHERE name = 'default'`).Scan(&defaultNet); err != nil {
		t.Fatalf("default network: %v", err)
	}
	var extern, meshID uuid.UUID
	if err := conn.QueryRow(ctx,
		`INSERT INTO targets (kind, name, address, port) VALUES ('external', 'svc', '203.0.113.7', 443) RETURNING id`).Scan(&extern); err != nil {
		t.Fatalf("insert target: %v", err)
	}
	if err := conn.QueryRow(ctx,
		`INSERT INTO mesh_groups (name, network_id) VALUES ('m1', $1) RETURNING id`, defaultNet).Scan(&meshID); err != nil {
		t.Fatalf("insert mesh: %v", err)
	}

	expectPgErr := func(what, code, sql string, args ...any) {
		t.Helper()
		_, err := conn.Exec(ctx, sql, args...)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != code {
			t.Errorf("%s: err = %v, want SQLSTATE %s", what, err, code)
		}
	}

	expectPgErr("agent without network", "23502",
		`INSERT INTO agents (id, site_id, hostname) VALUES ($1, $2, 'a1')`, uuid.New(), siteID)
	expectPgErr("direct probe without network", "23514",
		`INSERT INTO probe_configs (site_id, target_id, probe_type, interval_ms, timeout_ms) VALUES ($1, $2, 1, 60000, 5000)`,
		siteID, extern)
	expectPgErr("mesh template with network", "23514",
		`INSERT INTO probe_configs (mesh_id, network_id, probe_type, interval_ms, timeout_ms) VALUES ($1, $2, 1, 60000, 5000)`,
		meshID, defaultNet)
	expectPgErr("duplicate network name", "23505",
		`INSERT INTO networks (name) VALUES ('default')`)
}
