package store_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/devalexllc/polarbeam/internal/server/store"
)

func insertOutageEvent(
	t *testing.T,
	ctx context.Context,
	s *store.Store,
	id uuid.UUID,
	kind string,
	agentID uuid.UUID,
	probeID, targetID *uuid.UUID,
	opened time.Time,
	closed *time.Time,
) {
	t.Helper()
	if _, err := s.Pool().Exec(ctx, `
		INSERT INTO outage_events
		       (id, kind, agent_id, probe_id, target_id, probe_type,
		        opened_at, closed_at, open_error)
		VALUES ($1, $2, $3, $4, $5, CASE WHEN $4::uuid IS NULL THEN NULL ELSE 1 END,
		        $6, $7, 'test incident')`,
		id, kind, agentID, probeID, targetID, opened, closed); err != nil {
		t.Fatalf("insert outage %s: %v", id, err)
	}
}

func TestListOutagesStableIdentitiesAndRelatedRoutes(t *testing.T) {
	t.Parallel()
	ctx, s := newStore(t)
	f := buildNetFixture(t, ctx, s)
	var serviceTarget uuid.UUID
	if err := s.Pool().QueryRow(ctx, `SELECT id FROM targets WHERE name = 'svc'`).Scan(&serviceTarget); err != nil {
		t.Fatalf("service target: %v", err)
	}

	base := time.Now().UTC().Add(-8 * time.Hour).Truncate(time.Microsecond)
	liveOutage, offlineOutage := uuid.New(), uuid.New()
	orphanOutage, legacyOutage := uuid.New(), uuid.New()
	liveProbe, wrongProbe := uuid.New(), uuid.New()
	offlineProbe, orphanProbe, legacyRouteProbe := uuid.New(), uuid.New(), uuid.New()
	orphanAgent, orphanTarget := uuid.New(), uuid.New()

	liveClosed := base.Add(5 * time.Minute)
	insertOutageEvent(t, ctx, s, liveOutage, "probe_failing", f.aDef, &liveProbe, &f.tBDef, base, &liveClosed)
	insertOutageEvent(t, ctx, s, offlineOutage, "agent_offline", f.aMgmt, nil, nil, base.Add(2*time.Hour), nil)
	orphanClosed := base.Add(4*time.Hour + 5*time.Minute)
	insertOutageEvent(t, ctx, s, orphanOutage, "probe_failing", orphanAgent, &orphanProbe, &orphanTarget,
		base.Add(4*time.Hour), &orphanClosed)
	legacyClosed := base.Add(6*time.Hour + 5*time.Minute)
	insertOutageEvent(t, ctx, s, legacyOutage, "probe_failing", f.aDef, nil, &serviceTarget,
		base.Add(6*time.Hour), &legacyClosed)

	liveRoute, wrongRoute := uuid.New(), uuid.New()
	offlineRoute, orphanRoute, legacyRoute := uuid.New(), uuid.New(), uuid.New()
	insertPathQueryEvent(t, ctx, s, liveRoute, base.Add(time.Minute), f.aDef, liveProbe, f.tBDef, `[]`, `[]`)
	// The traceroute probe is a different config from the failing probe; exact
	// agent+target identity must still correlate it.
	insertPathQueryEvent(t, ctx, s, wrongRoute, base.Add(30*time.Second), f.aDef, wrongProbe, f.tBDef, `[]`, `[]`)
	insertPathQueryEvent(t, ctx, s, offlineRoute, base.Add(2*time.Hour+time.Minute), f.aMgmt, offlineProbe, f.tBMgmt, `[]`, `[]`)
	// Neither resource exists, so only the event tables' stable IDs can join
	// this deleted-resource history.
	insertPathQueryEvent(t, ctx, s, orphanRoute, base.Add(4*time.Hour+time.Minute), orphanAgent, orphanProbe, orphanTarget, `[]`, `[]`)
	// A legacy outage missing probe_id still correlates by its exact
	// agent+target identity.
	insertPathQueryEvent(t, ctx, s, legacyRoute, base.Add(6*time.Hour+time.Minute), f.aDef, legacyRouteProbe, serviceTarget, `[]`, `[]`)

	outages, _, err := s.ListOutages(ctx, 24*time.Hour, nil, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[uuid.UUID]store.OutageInfo, len(outages))
	for _, outage := range outages {
		byID[outage.ID] = outage
	}
	if len(byID) != 4 {
		t.Fatalf("outages = %d, want 4: %+v", len(byID), outages)
	}

	live := byID[liveOutage]
	if live.AgentID != f.aDef || live.ProbeID == nil || *live.ProbeID != liveProbe ||
		live.TargetID == nil || *live.TargetID != f.tBDef ||
		!slices.Equal(pathEventIDs(live.RelatedRoutes), []uuid.UUID{wrongRoute, liveRoute}) {
		t.Errorf("live probe outage identities/routes = %+v", live)
	}
	offline := byID[offlineOutage]
	if offline.AgentID != f.aMgmt || offline.ProbeID != nil || offline.TargetID != nil ||
		!slices.Equal(pathEventIDs(offline.RelatedRoutes), []uuid.UUID{offlineRoute}) {
		t.Errorf("offline outage identities/routes = %+v", offline)
	}
	orphan := byID[orphanOutage]
	if orphan.AgentID != orphanAgent || orphan.ProbeID == nil || *orphan.ProbeID != orphanProbe ||
		orphan.TargetID == nil || *orphan.TargetID != orphanTarget ||
		orphan.AgentHostname != "" || orphan.TargetName != nil ||
		!slices.Equal(pathEventIDs(orphan.RelatedRoutes), []uuid.UUID{orphanRoute}) {
		t.Errorf("deleted-resource outage identities/routes = %+v", orphan)
	}
	legacy := byID[legacyOutage]
	if legacy.ProbeID != nil || !slices.Contains(pathEventIDs(legacy.RelatedRoutes), legacyRoute) {
		t.Errorf("legacy missing-probe routes = %+v", legacy.RelatedRoutes)
	}
	withoutRoutes, _, err := s.ListOutages(ctx, 24*time.Hour, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, outage := range withoutRoutes {
		if len(outage.RelatedRoutes) != 0 {
			t.Errorf("opt-out outage %s unexpectedly has routes", outage.ID)
		}
	}
}

// TestListOutagesOpenBranchCap: the open branch carries a high safety cap so
// a pathological incident cannot make the 30s-polled endpoint unbounded —
// and the cut is reported, never silent.
func TestListOutagesOpenBranchCap(t *testing.T) {
	t.Parallel()
	ctx, s := newStore(t)
	f := buildNetFixture(t, ctx, s)

	// 2001 open events (one past the cap), opened one second apart.
	if _, err := s.Pool().Exec(ctx, `
		INSERT INTO outage_events (id, kind, agent_id, probe_id, target_id,
		                           probe_type, opened_at, closed_at, open_error)
		SELECT gen_random_uuid(), 'probe_failing', $1, gen_random_uuid(), $2, 1,
		       now() - make_interval(secs => i), NULL, 'flood'
		FROM generate_series(1, 2001) AS i`, f.aDef, f.tBDef); err != nil {
		t.Fatalf("seed outage flood: %v", err)
	}

	outages, truncation, err := s.ListOutages(ctx, 24*time.Hour, nil, false, nil)
	if err != nil {
		t.Fatalf("ListOutages: %v", err)
	}
	if !truncation.Open {
		t.Error("2001 open events: truncation.Open = false, want true")
	}
	if truncation.Closed {
		t.Error("no closed events: truncation.Closed = true, want false")
	}
	if len(outages) != 2000 {
		t.Fatalf("got %d events, want 2000 (cap)", len(outages))
	}
	for i := 1; i < len(outages); i++ {
		if outages[i].OpenedAt.After(outages[i-1].OpenedAt) {
			t.Fatalf("events not newest-first at index %d", i)
		}
	}

	// Closing the oldest brings the open count to the cap exactly: nothing
	// is cut, and the closed event still rides the closed branch.
	if _, err := s.Pool().Exec(ctx, `
		UPDATE outage_events SET closed_at = now()
		WHERE opened_at = (SELECT min(opened_at) FROM outage_events)`); err != nil {
		t.Fatalf("close oldest: %v", err)
	}
	outages, truncation, err = s.ListOutages(ctx, 24*time.Hour, nil, false, nil)
	if err != nil {
		t.Fatalf("ListOutages after close: %v", err)
	}
	if truncation.Open || truncation.Closed {
		t.Errorf("2000 open events: truncation = %+v, want neither branch cut", truncation)
	}
	if len(outages) != 2001 {
		t.Errorf("got %d events, want 2001 (2000 open + 1 recently closed)", len(outages))
	}
}

// TestListOutagesSiteFilter: the site filter matches an event through its
// agent's site OR its target's agent's site, sits inside both union
// branches so the closed cap counts per site, keeps a deleted-source event
// for a global caller as long as its target's agent still sits at the site,
// and drops that orphan for a scoped caller (the network predicate fails
// closed on a deleted agent).
func TestListOutagesSiteFilter(t *testing.T) {
	t.Parallel()
	ctx, s := newStore(t)
	f := buildNetFixture(t, ctx, s)
	now := time.Now()
	hourAgo := now.Add(-time.Hour)

	// e1: A → B, open. e2: B offline, open. e3: B → A, closed an hour ago.
	// e4: deleted agent → A, open — attributable only through its target.
	e1, e2, e3, e4 := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	p1, p3, p4 := uuid.New(), uuid.New(), uuid.New()
	orphanAgent := uuid.New()
	insertOutageEvent(t, ctx, s, e1, "probe_failing", f.aDef, &p1, &f.tBDef, now.Add(-4*time.Hour), nil)
	insertOutageEvent(t, ctx, s, e2, "agent_offline", f.bDef, nil, nil, now.Add(-3*time.Hour), nil)
	insertOutageEvent(t, ctx, s, e3, "probe_failing", f.bDef, &p3, &f.tADef, now.Add(-2*time.Hour), &hourAgo)
	insertOutageEvent(t, ctx, s, e4, "probe_failing", orphanAgent, &p4, &f.tADef, now.Add(-90*time.Minute), nil)

	ids := func(out []store.OutageInfo) []uuid.UUID {
		got := make([]uuid.UUID, 0, len(out))
		for _, o := range out {
			got = append(got, o.ID)
		}
		slices.SortFunc(got, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
		return got
	}
	sorted := func(want ...uuid.UUID) []uuid.UUID {
		slices.SortFunc(want, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
		return want
	}
	unknown := uuid.New()
	for _, tc := range []struct {
		name   string
		scope  []uuid.UUID
		site   *uuid.UUID
		want   []uuid.UUID
		closed bool
	}{
		{"unfiltered", nil, nil, sorted(e1, e2, e3, e4), false},
		{"site A (global)", nil, &f.siteA, sorted(e1, e3, e4), false},
		{"site B (global)", nil, &f.siteB, sorted(e1, e2, e3), false},
		{"unknown site", nil, &unknown, sorted(), false},
		{"site A (default-scoped drops the orphan)", []uuid.UUID{f.defaultNet}, &f.siteA, sorted(e1, e3), false},
	} {
		out, truncation, err := s.ListOutages(ctx, 24*time.Hour, tc.scope, false, tc.site)
		if err != nil {
			t.Fatalf("%s: ListOutages: %v", tc.name, err)
		}
		if got := ids(out); !slices.Equal(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
		if truncation.Open || truncation.Closed != tc.closed {
			t.Errorf("%s: truncation = %+v, want {Open:false Closed:%v}", tc.name, truncation, tc.closed)
		}
	}

	// Flood site B with 501 resolved offline events, all opened after e3:
	// the fleet-wide and the site-B closed branches are cut (e3, their
	// oldest resolved event, is the one dropped) and say so, while site A's
	// branch keeps e3 because the cap counts per site.
	if _, err := s.Pool().Exec(ctx, `
		INSERT INTO outage_events (id, kind, agent_id, opened_at, closed_at)
		SELECT gen_random_uuid(), 'agent_offline', $1,
		       now() - make_interval(secs => 600 + i), now() - make_interval(secs => i)
		FROM generate_series(1, 501) AS i`, f.bDef); err != nil {
		t.Fatalf("seed closed flood: %v", err)
	}
	for _, tc := range []struct {
		name       string
		site       *uuid.UUID
		wantClosed bool
		wantE3     bool
	}{
		{"unfiltered", nil, true, false},
		{"site B", &f.siteB, true, false},
		{"site A", &f.siteA, false, true},
	} {
		out, truncation, err := s.ListOutages(ctx, 24*time.Hour, nil, false, tc.site)
		if err != nil {
			t.Fatalf("%s: ListOutages: %v", tc.name, err)
		}
		closed := 0
		hasE3 := false
		for _, o := range out {
			if o.ClosedAt != nil {
				closed++
			}
			if o.ID == e3 {
				hasE3 = true
			}
		}
		if truncation.Closed != tc.wantClosed || truncation.Open {
			t.Errorf("%s: truncation = %+v, want {Open:false Closed:%v}", tc.name, truncation, tc.wantClosed)
		}
		if tc.wantClosed && closed != 500 {
			t.Errorf("%s: %d closed events returned, want exactly the 500 cap", tc.name, closed)
		}
		if hasE3 != tc.wantE3 {
			t.Errorf("%s: site A's resolved event present = %v, want %v", tc.name, hasE3, tc.wantE3)
		}
	}
}
