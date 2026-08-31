package outage_test

// DB-backed sweep test, gated on POLARBEAM_TEST_DB_URL (see
// internal/server/dbtest). Pins the network scoping of the min-interval
// attribution: a fast probe on one network plane must not shrink the
// silence window of an agent on another plane at the same site — the
// phantom-agent_offline failure mode the predicate exists to prevent.

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/devalexllc/polarbeam/internal/server/outage"
)

func TestSweepMinIntervalNetworkScoped(t *testing.T) {
	t.Parallel()
	ctx, pool := newPool(t)

	var defaultNet, mgmt uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT id FROM networks WHERE name = 'default'`).Scan(&defaultNet); err != nil {
		t.Fatalf("default network: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO networks (name) VALUES ('mgmt') RETURNING id`).Scan(&mgmt); err != nil {
		t.Fatalf("insert network: %v", err)
	}
	var siteID, extern uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO sites (name) VALUES ('site-a') RETURNING id`).Scan(&siteID); err != nil {
		t.Fatalf("insert site: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO targets (kind, name, address, port) VALUES ('external', 'svc', '203.0.113.7', 443) RETURNING id`).Scan(&extern); err != nil {
		t.Fatalf("insert target: %v", err)
	}

	// Both agents last produced a result 10 minutes ago and were last seen
	// 10 minutes ago (past the seen grace). The default plane probes every
	// second — its agent is genuinely silent. The mgmt plane probes hourly —
	// 10 minutes is well within 3× its own cadence, so its agent is only
	// offline if the fast default interval leaks across the network boundary.
	stale := time.Now().Add(-10 * time.Minute)
	aDef, aMgmt := uuid.New(), uuid.New()
	for _, a := range []struct {
		id, network uuid.UUID
		hostname    string
		intervalMS  int
	}{
		{aDef, defaultNet, "a-def", 1000},
		{aMgmt, mgmt, "a-mgmt", 3_600_000},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO agents (id, site_id, network_id, hostname, last_seen_at) VALUES ($1, $2, $3, $4, $5)`,
			a.id, siteID, a.network, a.hostname, stale); err != nil {
			t.Fatalf("insert agent %s: %v", a.hostname, err)
		}
		var probeID uuid.UUID
		if err := pool.QueryRow(ctx,
			`INSERT INTO probe_configs (site_id, target_id, network_id, probe_type, interval_ms, timeout_ms)
			 VALUES ($1, $2, $3, 1, $4, 500) RETURNING id`,
			siteID, extern, a.network, a.intervalMS).Scan(&probeID); err != nil {
			t.Fatalf("insert probe for %s: %v", a.hostname, err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO series_state (agent_id, probe_id, target_id, probe_type, last_status, last_time)
			 VALUES ($1, $2, $3, 1, 1, $4)`,
			a.id, probeID, extern, stale); err != nil {
			t.Fatalf("insert series_state for %s: %v", a.hostname, err)
		}
	}

	if err := outage.SweepOnce(ctx, pool, time.Now()); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}

	offline := func(agentID uuid.UUID) bool {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM outage_events WHERE agent_id = $1 AND kind = 'agent_offline' AND closed_at IS NULL`,
			agentID).Scan(&n); err != nil {
			t.Fatalf("count events: %v", err)
		}
		return n > 0
	}
	if !offline(aDef) {
		t.Errorf("default agent not marked offline despite 10 min of silence against a 1 s cadence")
	}
	if offline(aMgmt) {
		t.Errorf("mgmt agent marked offline: the default plane's 1 s interval leaked across the network boundary")
	}
}
