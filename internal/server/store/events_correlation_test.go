package store

import (
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
)

func routeIDs(routes []PathEventInfo) []uuid.UUID {
	out := make([]uuid.UUID, len(routes))
	for i := range routes {
		out[i] = routes[i].ID
	}
	return out
}

func TestCorrelateIncidentRoutes(t *testing.T) {
	opened := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	closed := opened.Add(20 * time.Minute)
	agentID, probeID, targetID := uuid.New(), uuid.New(), uuid.New()
	dst, target := "nyc", "edge"
	base := OutageInfo{
		Kind: "probe_failing", AgentID: agentID, ProbeID: &probeID, TargetID: &targetID,
		AgentHostname: "lon-1", SrcSite: "lon", DstSite: &dst, TargetName: &target,
		OpenedAt: opened, ClosedAt: &closed,
	}
	event := func(id uuid.UUID, at time.Time) PathEventInfo {
		return PathEventInfo{
			ID: id, Time: at, AgentID: agentID, ProbeID: probeID, TargetID: &targetID,
			AgentHostname: "lon-1", SrcSite: "lon", DstSite: &dst, TargetName: &target,
		}
	}
	nearestClose := event(uuid.New(), closed.Add(time.Minute))
	nearestOpen := event(uuid.New(), opened.Add(2*time.Minute))
	overlap := event(uuid.New(), opened.Add(10*time.Minute))
	farOpen := event(uuid.New(), opened.Add(-14*time.Minute))
	outOfWindow := event(uuid.New(), opened.Add(-16*time.Minute))
	differentProbe := event(uuid.New(), opened.Add(time.Minute))
	differentProbe.ProbeID = uuid.New()

	outages := []OutageInfo{base}
	correlateIncidentRoutes(outages, []PathEventInfo{
		farOpen, overlap, differentProbe, nearestClose, nearestOpen, nearestClose, outOfWindow,
	})
	if got, want := routeIDs(outages[0].RelatedRoutes), []uuid.UUID{nearestClose.ID, differentProbe.ID, nearestOpen.ID}; !slices.Equal(got, want) {
		t.Errorf("closed incident routes = %v, want nearest deduplicated %v", got, want)
	}

	open := base
	open.ClosedAt = nil
	outages = []OutageInfo{open}
	correlateIncidentRoutes(outages, []PathEventInfo{nearestClose, nearestOpen, overlap, farOpen})
	if got, want := routeIDs(outages[0].RelatedRoutes), []uuid.UUID{nearestOpen.ID, overlap.ID, farOpen.ID}; !slices.Equal(got, want) {
		t.Errorf("open incident routes = %v, want opening-window-only %v", got, want)
	}

	legacy := base
	legacy.AgentID, legacy.ProbeID, legacy.TargetID = uuid.Nil, nil, nil
	legacy.ClosedAt = nil
	legacyRoute := event(uuid.New(), opened.Add(time.Minute))
	legacyRoute.AgentID, legacyRoute.ProbeID, legacyRoute.TargetID = uuid.Nil, uuid.Nil, nil
	outages = []OutageInfo{legacy}
	correlateIncidentRoutes(outages, []PathEventInfo{legacyRoute})
	if got := routeIDs(outages[0].RelatedRoutes); !slices.Equal(got, []uuid.UUID{legacyRoute.ID}) {
		t.Errorf("legacy label fallback = %v, want %s", got, legacyRoute.ID)
	}

	if !incidentRouteMatches(base, differentProbe) {
		t.Error("different failure/traceroute probe IDs hid an exact agent+target route")
	}
	wrongTarget := differentProbe
	wrongTargetID := uuid.New()
	wrongTarget.TargetID = &wrongTargetID
	if incidentRouteMatches(base, wrongTarget) {
		t.Error("usable mismatched target IDs fell back to identical labels")
	}

	unrelated := legacyRoute
	unrelated.SrcSite, unrelated.AgentHostname = "syd", "syd-1"
	if incidentRouteMatches(legacy, unrelated) {
		t.Error("legacy fallback matched different source labels")
	}
}
