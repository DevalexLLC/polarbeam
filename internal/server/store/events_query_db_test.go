package store_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/devalexllc/polarbeam/internal/server/store"
)

func insertPathQueryEvent(
	t *testing.T,
	ctx context.Context,
	s *store.Store,
	id uuid.UUID,
	when time.Time,
	agentID, probeID, targetID uuid.UUID,
	oldHops, newHops string,
) {
	t.Helper()
	if _, err := s.Pool().Exec(ctx, `
		INSERT INTO path_events
		       (id, time, agent_id, probe_id, target_id,
		        old_path_hash, new_path_hash, old_hops, new_hops)
		VALUES ($1, $2, $3, $4, $5, '\x01', '\x02', $6::jsonb, $7::jsonb)`,
		id, when, agentID, probeID, targetID, oldHops, newHops); err != nil {
		t.Fatalf("insert path event %s: %v", id, err)
	}
}

func pathEventIDs(events []store.PathEventInfo) []uuid.UUID {
	out := make([]uuid.UUID, len(events))
	for i := range events {
		out[i] = events[i].ID
	}
	return out
}

func TestQueryPathEventsFilteringSortingAndPaging(t *testing.T) {
	ctx, s := newStore(t)
	f := buildNetFixture(t, ctx, s)
	var serviceID uuid.UUID
	if err := s.Pool().QueryRow(ctx, `SELECT id FROM targets WHERE name = 'svc'`).Scan(&serviceID); err != nil {
		t.Fatalf("service target: %v", err)
	}

	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	ids := []uuid.UUID{
		uuid.MustParse("00000000-0000-4000-8000-000000000010"),
		uuid.MustParse("00000000-0000-4000-8000-000000000011"),
		uuid.MustParse("00000000-0000-4000-8000-000000000012"),
		uuid.MustParse("00000000-0000-4000-8000-000000000013"),
	}
	probeIDs := []uuid.UUID{uuid.New(), uuid.New(), uuid.New(), uuid.New()}
	// Reordered/duplicated addresses and RTT changes do not change a TTL.
	insertPathQueryEvent(t, ctx, s, ids[0], base, f.aDef, probeIDs[0], f.tBDef,
		`[{"ttl":1,"addrs":["10.0.0.2","10.0.0.1","10.0.0.1"],"rtt_us":[1]},
		  {"ttl":2,"addrs":[],"rtt_us":[]},
		  {"ttl":3000000000,"addrs":["10.0.0.3"],"rtt_us":[1]}]`,
		`[{"ttl":1,"addrs":["10.0.0.1","10.0.0.2"],"rtt_us":[999]},
		  {"ttl":2,"addrs":[],"rtt_us":[]},
		  {"ttl":3000000000,"addrs":["10.0.0.3"],"rtt_us":[999]}]`)
	// One removed silent TTL plus one added silent TTL = two changes.
	insertPathQueryEvent(t, ctx, s, ids[1], base, f.aDef, probeIDs[1], serviceID,
		`[{"ttl":1,"addrs":["10.0.0.1"]},{"ttl":2,"addrs":[]}]`,
		`[{"ttl":1,"addrs":["10.0.0.1"]},{"ttl":3,"addrs":[]}]`)
	// A changed address set at one existing TTL = one change.
	insertPathQueryEvent(t, ctx, s, ids[2], base.Add(-time.Minute), f.aMgmt, probeIDs[2], f.tBMgmt,
		`[{"ttl":1,"addrs":["10.0.0.1","10.0.0.2"]}]`,
		`[{"ttl":1,"addrs":["10.0.0.2","10.0.0.3"]}]`)
	// Deleted display resources retain all stable event identities.
	orphanAgent, orphanTarget := uuid.New(), uuid.New()
	insertPathQueryEvent(t, ctx, s, ids[3], base.Add(-2*time.Minute), orphanAgent, probeIDs[3], orphanTarget,
		`[]`, `[]`)

	query := func(filter store.PathEventFilter) ([]store.PathEventInfo, int64, bool) {
		t.Helper()
		events, total, truncated, err := s.QueryPathEvents(ctx, 24*time.Hour, filter)
		if err != nil {
			t.Fatalf("QueryPathEvents(%+v): %v", filter, err)
		}
		return events, total, truncated
	}
	baseFilter := store.PathEventFilter{Sort: "time", Order: "desc", Limit: 100}
	events, total, truncated := query(baseFilter)
	if total != 4 || truncated || !slices.Equal(pathEventIDs(events), []uuid.UUID{ids[1], ids[0], ids[2], ids[3]}) {
		t.Fatalf("default query = ids %v total %d truncated %v", pathEventIDs(events), total, truncated)
	}
	wantChanges := map[uuid.UUID]int{ids[0]: 0, ids[1]: 2, ids[2]: 1, ids[3]: 0}
	for _, event := range events {
		if event.ChangedHops != wantChanges[event.ID] {
			t.Errorf("changed_hops[%s] = %d, want %d", event.ID, event.ChangedHops, wantChanges[event.ID])
		}
	}
	orphan := events[3]
	if orphan.AgentID != orphanAgent || orphan.ProbeID != probeIDs[3] || orphan.TargetID == nil ||
		*orphan.TargetID != orphanTarget || orphan.AgentHostname != "" || orphan.SrcSite != "" ||
		orphan.DstSite != nil || orphan.TargetName != nil {
		t.Errorf("orphan event lost stable identities or gained labels: %+v", orphan)
	}

	searches := []struct {
		query string
		want  []uuid.UUID
	}{
		{query: "A-DEF", want: []uuid.UUID{ids[1], ids[0]}},
		{query: "site-a", want: []uuid.UUID{ids[1], ids[0], ids[2]}},
		{query: "site-b", want: []uuid.UUID{ids[0], ids[2]}},
		{query: "svc", want: []uuid.UUID{ids[1]}},
		{query: "svc%_", want: []uuid.UUID{}},
	}
	for _, tt := range searches {
		filter := baseFilter
		filter.Query = tt.query
		got, gotTotal, gotTruncated := query(filter)
		if gotTruncated || gotTotal != int64(len(tt.want)) || !slices.Equal(pathEventIDs(got), tt.want) {
			t.Errorf("search %q = %v total %d truncated %v, want %v", tt.query, pathEventIDs(got), gotTotal, gotTruncated, tt.want)
		}
	}

	sorts := []struct {
		name string
		want []uuid.UUID
	}{
		{name: "time", want: []uuid.UUID{ids[3], ids[2], ids[0], ids[1]}},
		{name: "agent", want: []uuid.UUID{ids[3], ids[0], ids[1], ids[2]}},
		{name: "source", want: []uuid.UUID{ids[3], ids[0], ids[1], ids[2]}},
		{name: "destination", want: []uuid.UUID{ids[3], ids[0], ids[2], ids[1]}},
		{name: "changes", want: []uuid.UUID{ids[0], ids[3], ids[2], ids[1]}},
	}
	for _, tt := range sorts {
		filter := baseFilter
		filter.Sort, filter.Order = tt.name, "asc"
		got, _, _ := query(filter)
		if !slices.Equal(pathEventIDs(got), tt.want) {
			t.Errorf("sort %s asc = %v, want %v", tt.name, pathEventIDs(got), tt.want)
		}
	}

	firstFilter := baseFilter
	firstFilter.Limit = 2
	first, firstTotal, _ := query(firstFilter)
	secondFilter := firstFilter
	secondFilter.Offset = 2
	second, secondTotal, _ := query(secondFilter)
	joined := append(pathEventIDs(first), pathEventIDs(second)...)
	if firstTotal != 4 || secondTotal != 4 || !slices.Equal(joined, []uuid.UUID{ids[1], ids[0], ids[2], ids[3]}) {
		t.Errorf("adjacent pages = %v totals %d/%d", joined, firstTotal, secondTotal)
	}
	finalFilter := firstFilter
	finalFilter.Offset = 3
	final, finalTotal, _ := query(finalFilter)
	if finalTotal != 4 || !slices.Equal(pathEventIDs(final), []uuid.UUID{ids[3]}) {
		t.Errorf("final page = %v total %d", pathEventIDs(final), finalTotal)
	}
	emptyFilter := firstFilter
	emptyFilter.Offset = 4
	empty, emptyTotal, _ := query(emptyFilter)
	if emptyTotal != 4 || len(empty) != 0 {
		t.Errorf("empty page = %v total %d", pathEventIDs(empty), emptyTotal)
	}

	scopedFilter := baseFilter
	scopedFilter.Networks = []uuid.UUID{f.mgmt}
	scoped, scopedTotal, _ := query(scopedFilter)
	if scopedTotal != 1 || !slices.Equal(pathEventIDs(scoped), []uuid.UUID{ids[2]}) {
		t.Errorf("mgmt scope = %v total %d, want event %s", pathEventIDs(scoped), scopedTotal, ids[2])
	}

	for _, bad := range []store.PathEventFilter{
		{Sort: "bogus", Order: "asc", Limit: 1},
		{Sort: "time", Order: "sideways", Limit: 1},
		{Sort: "time", Order: "asc", Limit: 0},
		{Sort: "time", Order: "asc", Limit: 101},
		{Sort: "time", Order: "asc", Limit: 1, Offset: -1},
	} {
		if _, _, _, err := s.QueryPathEvents(ctx, time.Hour, bad); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("bad filter %+v: err = %v, want ErrInvalid", bad, err)
		}
	}
}

func TestQueryPathEventsSafetyCapAndDefaultIndex(t *testing.T) {
	ctx, s := newStore(t)
	bulkAgent := enrollNetAgent(t, ctx, s, "bulk-site", "bulk-agent", nil)
	target := agentTargetID(t, ctx, s, bulkAgent)
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	if _, err := s.Pool().Exec(ctx, `
		INSERT INTO path_events
		       (time, agent_id, probe_id, target_id,
		        old_path_hash, new_path_hash, old_hops, new_hops)
		SELECT $1::timestamptz - g * interval '1 second', $2, gen_random_uuid(), $3,
		       '\x01', '\x02', '[]', '[]'
		  FROM generate_series(1, 501) g`, base, bulkAgent, target); err != nil {
		t.Fatalf("insert bulk path events: %v", err)
	}
	legacy, err := s.ListPathEvents(ctx, 24*time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(legacy) != 500 {
		t.Fatalf("legacy newest-500 length = %d, want 500", len(legacy))
	}
	if !legacy[0].Time.Equal(base.Add(-time.Second)) ||
		!legacy[499].Time.Equal(base.Add(-500*time.Second)) {
		t.Errorf("legacy newest-500 = first %v last %v", legacy[0].Time, legacy[499].Time)
	}

	filter := store.PathEventFilter{
		Query: "bulk-agent", Sort: "time", Order: "asc", Limit: 100,
	}
	events, total, truncated, err := s.QueryPathEvents(ctx, 24*time.Hour, filter)
	if err != nil {
		t.Fatal(err)
	}
	if total != 500 || !truncated || len(events) != 100 || !events[0].Time.Equal(base.Add(-500*time.Second)) {
		t.Errorf("capped first page = len %d total %d truncated %v first %v", len(events), total, truncated, events[0].Time)
	}
	filter.Offset = 499
	events, total, truncated, err = s.QueryPathEvents(ctx, 24*time.Hour, filter)
	if err != nil {
		t.Fatal(err)
	}
	if total != 500 || !truncated || len(events) != 1 || !events[0].Time.Equal(base.Add(-time.Second)) {
		t.Errorf("capped final page = len %d total %d truncated %v", len(events), total, truncated)
	}

	conn, err := s.Pool().Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire plan connection: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET enable_seqscan = off`); err != nil {
		t.Fatalf("disable seqscan: %v", err)
	}
	rows, err := conn.Query(ctx, `
		EXPLAIN SELECT pe.id
		  FROM path_events pe
		 WHERE pe.time > now() - interval '24 hours'
		 ORDER BY pe.time DESC, pe.id DESC
		 LIMIT 501`)
	if err != nil {
		t.Fatalf("EXPLAIN default path event query: %v", err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, line)
	}
	joined := strings.Join(plan, "\n")
	if !strings.Contains(joined, "path_events_time_id_idx") {
		t.Errorf("default path query does not use stable paging index:\n%s", joined)
	}
}
