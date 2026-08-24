package store_test

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/devalexllc/polarbeam/internal/server/store"
)

func sortedUUIDs(ids ...uuid.UUID) []uuid.UUID {
	out := append([]uuid.UUID(nil), ids...)
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

func agentInventoryIDs(rows []store.AgentListInfo) []uuid.UUID {
	out := make([]uuid.UUID, len(rows))
	for i := range rows {
		out[i] = rows[i].ID
	}
	return out
}

func targetInventoryIDs(rows []store.OperationalTargetInfo) []uuid.UUID {
	out := make([]uuid.UUID, len(rows))
	for i := range rows {
		out[i] = rows[i].ID
	}
	return out
}

func seedInventoryAgents(t *testing.T, ctx context.Context, s *store.Store) (netFixture, time.Time) {
	t.Helper()
	f := buildNetFixture(t, ctx, s)
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	if _, err := s.Pool().Exec(ctx, `
		UPDATE agents
		   SET hostname = CASE id
		         WHEN $1 THEN 'tie' WHEN $2 THEN 'tie'
		         WHEN $3 THEN 'zeta' ELSE 'no-data' END,
		       version = CASE id
		         WHEN $1 THEN 'v2' WHEN $2 THEN 'v1'
		         WHEN $3 THEN 'v3' ELSE '' END,
		       last_seen_at = CASE
		         WHEN id = $4 THEN NULL
		         WHEN id = $3 THEN $5::timestamptz - interval '1 hour'
		         ELSE $5::timestamptz END
		 WHERE id = ANY($6::uuid[])`,
		f.aDef, f.aMgmt, f.bDef, f.bMgmt, base,
		[]uuid.UUID{f.aDef, f.aMgmt, f.bDef, f.bMgmt}); err != nil {
		t.Fatalf("update inventory agents: %v", err)
	}
	openID := uuid.New()
	if _, err := s.Pool().Exec(ctx, `
		INSERT INTO outage_events
		       (id, kind, agent_id, probe_id, target_id, probe_type, opened_at)
		VALUES ($1, 'probe_degraded', $2, $3, $4, 1, $5)`,
		openID, f.aDef, f.directID, f.tBDef, base); err != nil {
		t.Fatalf("insert degraded outage: %v", err)
	}
	if _, err := s.Pool().Exec(ctx, `
		INSERT INTO series_state
		       (agent_id, probe_id, target_id, probe_type, last_status,
		        last_time, open_event_id)
		VALUES ($1, $2, $3, 1, 2, $4, $5)`,
		f.aDef, f.directID, f.tBDef, base, openID); err != nil {
		t.Fatalf("insert degraded series: %v", err)
	}
	if _, err := s.Pool().Exec(ctx, `
		INSERT INTO outage_events (kind, agent_id, opened_at)
		VALUES ('agent_offline', $1, $2)`, f.bDef, base); err != nil {
		t.Fatalf("insert offline outage: %v", err)
	}
	return f, base
}

func TestQueryAgentsFilteringSortingPagingAndScope(t *testing.T) {
	ctx, s := newStore(t)
	f, _ := seedInventoryAgents(t, ctx, s)
	query := func(filter store.AgentInventoryFilter) ([]store.AgentListInfo, store.AgentInventorySummary) {
		t.Helper()
		rows, summary, err := s.QueryAgents(ctx, filter)
		if err != nil {
			t.Fatalf("QueryAgents(%+v): %v", filter, err)
		}
		return rows, summary
	}
	baseFilter := store.AgentInventoryFilter{Sort: "health", Order: "asc", Limit: 100}
	rows, summary := query(baseFilter)
	if want := []uuid.UUID{f.bDef, f.aDef, f.bMgmt, f.aMgmt}; !slices.Equal(agentInventoryIDs(rows), want) {
		t.Errorf("health order = %v, want %v", agentInventoryIDs(rows), want)
	}
	if summary != (store.AgentInventorySummary{Total: 4, Offline: 1, Degraded: 1, Healthy: 1, NoData: 1}) {
		t.Errorf("agent summary = %+v", summary)
	}

	for _, tt := range []struct {
		name string
		want []uuid.UUID
	}{
		{name: "hostname", want: append([]uuid.UUID{f.bMgmt}, append(sortedUUIDs(f.aDef, f.aMgmt), f.bDef)...)},
		{name: "site", want: append(sortedUUIDs(f.aDef, f.aMgmt), sortedUUIDs(f.bDef, f.bMgmt)...)},
		{name: "network", want: append(sortedUUIDs(f.aDef, f.bDef), sortedUUIDs(f.aMgmt, f.bMgmt)...)},
		{name: "health", want: []uuid.UUID{f.bDef, f.aDef, f.bMgmt, f.aMgmt}},
		{name: "last_seen", want: append([]uuid.UUID{f.bDef}, append(sortedUUIDs(f.aDef, f.aMgmt), f.bMgmt)...)},
		{name: "version", want: []uuid.UUID{f.bMgmt, f.aMgmt, f.aDef, f.bDef}},
	} {
		filter := baseFilter
		filter.Sort = tt.name
		got, _ := query(filter)
		if !slices.Equal(agentInventoryIDs(got), tt.want) {
			t.Errorf("sort %s asc = %v, want %v", tt.name, agentInventoryIDs(got), tt.want)
		}
		filter.Order = "desc"
		got, _ = query(filter)
		wantDesc := append([]uuid.UUID(nil), tt.want...)
		slices.Reverse(wantDesc)
		if tt.name == "last_seen" {
			wantDesc = append(sortedUUIDs(f.aDef, f.aMgmt), f.bDef, f.bMgmt)
			slices.Reverse(wantDesc[:2])
		}
		if !slices.Equal(agentInventoryIDs(got), wantDesc) {
			t.Errorf("sort %s desc = %v, want %v", tt.name, agentInventoryIDs(got), wantDesc)
		}
	}

	filter := baseFilter
	filter.Query = "SITE-B"
	rows, summary = query(filter)
	if summary.Total != 2 || summary.Offline != 1 || summary.NoData != 1 ||
		!slices.Equal(agentInventoryIDs(rows), []uuid.UUID{f.bDef, f.bMgmt}) {
		t.Errorf("site search = ids %v summary %+v", agentInventoryIDs(rows), summary)
	}
	filter.Query = "site%_"
	rows, summary = query(filter)
	if len(rows) != 0 || summary.Total != 0 {
		t.Errorf("literal wildcard search = ids %v summary %+v", agentInventoryIDs(rows), summary)
	}
	filter.Query, filter.Health = "", store.AgentHealthDegraded
	rows, summary = query(filter)
	if !slices.Equal(agentInventoryIDs(rows), []uuid.UUID{f.aDef}) ||
		summary != (store.AgentInventorySummary{Total: 1, Degraded: 1}) {
		t.Errorf("degraded filter = ids %v summary %+v", agentInventoryIDs(rows), summary)
	}

	filter = baseFilter
	filter.Networks = []uuid.UUID{f.mgmt}
	rows, summary = query(filter)
	if !slices.Equal(agentInventoryIDs(rows), []uuid.UUID{f.bMgmt, f.aMgmt}) ||
		summary != (store.AgentInventorySummary{Total: 2, Healthy: 1, NoData: 1}) {
		t.Errorf("mgmt scope = ids %v summary %+v", agentInventoryIDs(rows), summary)
	}

	filter = baseFilter
	filter.Sort, filter.Limit = "hostname", 1
	var paged []uuid.UUID
	for offset := range 4 {
		filter.Offset = offset
		page, pageSummary := query(filter)
		if pageSummary.Total != 4 || len(page) != 1 {
			t.Fatalf("agent page %d = len %d summary %+v", offset, len(page), pageSummary)
		}
		paged = append(paged, page[0].ID)
	}
	wantPaged := append([]uuid.UUID{f.bMgmt}, append(sortedUUIDs(f.aDef, f.aMgmt), f.bDef)...)
	if !slices.Equal(paged, wantPaged) {
		t.Errorf("adjacent agent pages = %v, want %v", paged, wantPaged)
	}
	filter.Offset = 4
	if page, pageSummary := query(filter); len(page) != 0 || pageSummary.Total != 4 {
		t.Errorf("past-end agent page = %v summary %+v", page, pageSummary)
	}

	for _, bad := range []store.AgentInventoryFilter{
		{Sort: "bogus", Order: "asc", Limit: 1},
		{Sort: "health", Order: "sideways", Limit: 1},
		{Sort: "health", Order: "asc", Health: "bogus", Limit: 1},
		{Sort: "health", Order: "asc", Limit: 0},
		{Sort: "health", Order: "asc", Limit: 101},
		{Sort: "health", Order: "asc", Limit: 1, Offset: -1},
	} {
		if _, _, err := s.QueryAgents(ctx, bad); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("bad agent filter %+v: err = %v, want ErrInvalid", bad, err)
		}
	}
}

func seedInventoryTargets(t *testing.T, ctx context.Context, s *store.Store) (netFixture, uuid.UUID, uuid.UUID) {
	t.Helper()
	f := buildNetFixture(t, ctx, s)
	mgmtTarget, err := s.UpsertExternalTarget(ctx, "mgmt-svc", "198.51.100.20", 8443, "", &f.mgmt, nil)
	if err != nil {
		t.Fatalf("create mgmt target: %v", err)
	}
	if _, err := s.AddDirectProbe(ctx, "site-a", "mgmt-svc", f.mgmt, netProbeSettings, false, "test", nil); err != nil {
		t.Fatalf("add disabled mgmt probe: %v", err)
	}
	if _, err := s.AddDirectProbe(ctx, "site-b", "svc", f.mgmt, netProbeSettings, true, "test", nil); err != nil {
		t.Fatalf("add enabled mgmt probe: %v", err)
	}
	foreignTarget, err := s.UpsertExternalTarget(ctx, "default-only", "198.51.100.30", 9443, "", &f.defaultNet, nil)
	if err != nil {
		t.Fatalf("create default target: %v", err)
	}
	var svc uuid.UUID
	if err := s.Pool().QueryRow(ctx, `SELECT id FROM targets WHERE name = 'svc'`).Scan(&svc); err != nil {
		t.Fatalf("svc target: %v", err)
	}
	for _, event := range []struct {
		agent, probe uuid.UUID
	}{{f.aDef, f.directID}, {f.aMgmt, uuid.New()}} {
		if _, err := s.Pool().Exec(ctx, `
			INSERT INTO outage_events
			       (kind, agent_id, probe_id, target_id, probe_type, opened_at)
			VALUES ('probe_failing', $1, $2, $3, 1, now())`,
			event.agent, event.probe, svc); err != nil {
			t.Fatalf("insert target incident: %v", err)
		}
	}
	if _, err := s.Pool().Exec(ctx, `UPDATE targets SET created_at = $1`,
		time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)); err != nil {
		t.Fatalf("tie target creation times: %v", err)
	}
	return f, mgmtTarget, foreignTarget
}

func TestQueryOperationalTargetsAggregatesSortingPagingAndScope(t *testing.T) {
	ctx, s := newStore(t)
	f, mgmtTarget, foreignTarget := seedInventoryTargets(t, ctx, s)
	query := func(filter store.TargetInventoryFilter) ([]store.OperationalTargetInfo, store.TargetInventorySummary) {
		t.Helper()
		rows, summary, err := s.QueryOperationalTargets(ctx, filter)
		if err != nil {
			t.Fatalf("QueryOperationalTargets(%+v): %v", filter, err)
		}
		return rows, summary
	}
	baseFilter := store.TargetInventoryFilter{Sort: "name", Order: "asc", Limit: 100}
	rows, summary := query(baseFilter)
	if summary != (store.TargetInventorySummary{
		Total: 7, External: 3, Agent: 4, Incident: 1, Unprobed: 4, NoIncidents: 2,
	}) {
		t.Fatalf("target summary = %+v", summary)
	}
	var svc *store.OperationalTargetInfo
	for i := range rows {
		if rows[i].Name == "svc" {
			svc = &rows[i]
		}
	}
	if svc == nil || svc.ProbeCount != 2 || svc.EnabledProbeCount != 2 ||
		svc.OpenIncidents != 2 || svc.Status != store.TargetStatusIncident ||
		!slices.Equal(svc.ProbingSites, []string{"site-a", "site-b"}) {
		t.Errorf("unscoped svc aggregate = %+v", svc)
	}
	for _, target := range rows {
		if target.Kind == "agent" && (target.AgentID == nil || target.AgentSite == nil || target.AgentHostname == nil) {
			t.Errorf("agent target missing stable identity/evidence: %+v", target)
		}
	}

	for _, sortName := range []string{"name", "kind", "status", "created", "probes"} {
		for _, order := range []string{"asc", "desc"} {
			filter := baseFilter
			filter.Sort, filter.Order = sortName, order
			sortedRows, gotSummary := query(filter)
			if len(sortedRows) != 7 || gotSummary.Total != 7 {
				t.Fatalf("sort %s %s = len %d summary %+v", sortName, order, len(sortedRows), gotSummary)
			}
			if !slices.IsSortedFunc(sortedRows, func(a, b store.OperationalTargetInfo) int {
				var cmp int
				switch sortName {
				case "name":
					cmp = strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
				case "kind":
					cmp = strings.Compare(a.Kind, b.Kind)
				case "status":
					rank := map[string]int{store.TargetStatusIncident: 0, store.TargetStatusUnprobed: 1, store.TargetStatusNoIncidents: 2}
					cmp = rank[a.Status] - rank[b.Status]
				case "created":
					cmp = a.CreatedAt.Compare(b.CreatedAt)
				case "probes":
					cmp = int(a.ProbeCount - b.ProbeCount)
				}
				if cmp == 0 {
					cmp = strings.Compare(a.ID.String(), b.ID.String())
				}
				if order == "desc" {
					cmp = -cmp
				}
				return cmp
			}) {
				t.Errorf("sort %s %s is not primary-value/id ordered: %v",
					sortName, order, targetInventoryIDs(sortedRows))
			}
		}
	}

	filter := baseFilter
	filter.Query = mgmtTarget.String()
	rows, summary = query(filter)
	if summary.Total != 1 || len(rows) != 1 || rows[0].ID != mgmtTarget {
		t.Errorf("stable-id search = ids %v summary %+v", targetInventoryIDs(rows), summary)
	}
	filter.Query = "8443"
	rows, summary = query(filter)
	if summary.Total != 1 || len(rows) != 1 || rows[0].ID != mgmtTarget {
		t.Errorf("port search = ids %v summary %+v", targetInventoryIDs(rows), summary)
	}
	filter.Query = "site-b"
	rows, summary = query(filter)
	if summary.Total == 0 {
		t.Error("probing/agent site search returned no targets")
	}
	filter.Query = "site%_"
	rows, summary = query(filter)
	if len(rows) != 0 || summary.Total != 0 {
		t.Errorf("literal wildcard target search = ids %v summary %+v", targetInventoryIDs(rows), summary)
	}
	filter.Query, filter.Kind, filter.Status = "", "external", store.TargetStatusUnprobed
	rows, summary = query(filter)
	if summary.Total != 2 || summary.External != 2 || summary.Unprobed != 2 {
		t.Errorf("target filters = ids %v summary %+v", targetInventoryIDs(rows), summary)
	}

	filter = baseFilter
	filter.Networks = []uuid.UUID{f.mgmt}
	rows, summary = query(filter)
	if summary != (store.TargetInventorySummary{
		Total: 4, External: 2, Agent: 2, Incident: 1, Unprobed: 3,
	}) {
		t.Errorf("mgmt target summary = %+v", summary)
	}
	if slices.Contains(targetInventoryIDs(rows), foreignTarget) {
		t.Errorf("foreign target leaked into mgmt scope: %v", targetInventoryIDs(rows))
	}
	for _, target := range rows {
		if target.Name == "svc" {
			if target.ProbeCount != 1 || target.EnabledProbeCount != 1 ||
				target.OpenIncidents != 1 || !slices.Equal(target.ProbingSites, []string{"site-b"}) {
				t.Errorf("scoped svc aggregate leaked counts/sites: %+v", target)
			}
		}
		if target.Kind == "agent" && target.Network != "mgmt" {
			t.Errorf("foreign agent target leaked: %+v", target)
		}
	}

	filter = baseFilter
	filter.Sort, filter.Limit = "created", 2
	var paged []uuid.UUID
	for offset := 0; offset < 7; offset += 2 {
		filter.Offset = offset
		page, pageSummary := query(filter)
		if pageSummary.Total != 7 {
			t.Fatalf("target page %d summary = %+v", offset, pageSummary)
		}
		paged = append(paged, targetInventoryIDs(page)...)
	}
	wantPaged := sortedUUIDs(paged...)
	if !slices.Equal(paged, wantPaged) {
		t.Errorf("stable target pages = %v, want ID order %v", paged, wantPaged)
	}
	filter.Offset = 8
	if page, pageSummary := query(filter); len(page) != 0 || pageSummary.Total != 7 {
		t.Errorf("past-end target page = %v summary %+v", page, pageSummary)
	}

	for _, bad := range []store.TargetInventoryFilter{
		{Sort: "bogus", Order: "asc", Limit: 1},
		{Sort: "name", Order: "sideways", Limit: 1},
		{Sort: "name", Order: "asc", Kind: "bogus", Limit: 1},
		{Sort: "name", Order: "asc", Status: "bogus", Limit: 1},
		{Sort: "name", Order: "asc", Limit: 0},
		{Sort: "name", Order: "asc", Limit: 101},
		{Sort: "name", Order: "asc", Limit: 1, Offset: -1},
	} {
		if _, _, err := s.QueryOperationalTargets(ctx, bad); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("bad target filter %+v: err = %v, want ErrInvalid", bad, err)
		}
	}
}
