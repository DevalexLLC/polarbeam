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
	// Identical live labels and target, but the wrong probe ID must not match.
	insertPathQueryEvent(t, ctx, s, wrongRoute, base.Add(30*time.Second), f.aDef, wrongProbe, f.tBDef, `[]`, `[]`)
	insertPathQueryEvent(t, ctx, s, offlineRoute, base.Add(2*time.Hour+time.Minute), f.aMgmt, offlineProbe, f.tBMgmt, `[]`, `[]`)
	// Neither resource exists, so only the event tables' stable IDs can join
	// this deleted-resource history.
	insertPathQueryEvent(t, ctx, s, orphanRoute, base.Add(4*time.Hour+time.Minute), orphanAgent, orphanProbe, orphanTarget, `[]`, `[]`)
	// A legacy outage missing probe_id falls back to its still-stable source
	// and external-target labels.
	insertPathQueryEvent(t, ctx, s, legacyRoute, base.Add(6*time.Hour+time.Minute), f.aDef, legacyRouteProbe, serviceTarget, `[]`, `[]`)

	outages, err := s.ListOutages(ctx, 24*time.Hour, nil)
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
		!slices.Equal(pathEventIDs(live.RelatedRoutes), []uuid.UUID{liveRoute}) {
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
		t.Errorf("legacy label fallback routes = %+v", legacy.RelatedRoutes)
	}
}
