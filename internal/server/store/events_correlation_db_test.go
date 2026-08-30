package store_test

import (
	"context"
	"slices"
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

	outages, _, err := s.ListOutages(ctx, 24*time.Hour, nil, true)
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
	withoutRoutes, _, err := s.ListOutages(ctx, 24*time.Hour, nil, false)
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

	outages, truncated, err := s.ListOutages(ctx, 24*time.Hour, nil, false)
	if err != nil {
		t.Fatalf("ListOutages: %v", err)
	}
	if !truncated {
		t.Error("2001 open events: truncated = false, want true")
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
	outages, truncated, err = s.ListOutages(ctx, 24*time.Hour, nil, false)
	if err != nil {
		t.Fatalf("ListOutages after close: %v", err)
	}
	if truncated {
		t.Error("2000 open events: truncated = true, want false")
	}
	if len(outages) != 2001 {
		t.Errorf("got %d events, want 2001 (2000 open + 1 recently closed)", len(outages))
	}
}
