package store_test

// DB-backed tests for the SCOPED WRITE surface added with the network_admin
// role: target ownership (0019), plane-qualified path thresholds (0020), and
// the per-network overlay (0021).
//
// The read-side counterpart is tenantscope_db_test.go. The rule these pin is
// narrower than "a tenant sees less": a tenant admin must be unable to WRITE
// another plane's rows, and the refusal must be ErrNotFound — the same error
// a name that never existed produces — so the write surface cannot be used to
// enumerate what else the control plane carries.

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/devalexllc/polarbeam/internal/server/store"
)

func TestTargetOwnershipScopedWrites(t *testing.T) {
	t.Parallel()
	ctx, s := newStore(t)
	f := buildNetFixture(t, ctx, s)
	mgmtScope := []uuid.UUID{f.mgmt}
	defScope := []uuid.UUID{f.defaultNet}

	// buildNetFixture already created the global target "svc" (network nil).
	if _, err := s.UpsertExternalTarget(ctx, "mgmt-svc", "203.0.113.20", 443, "", &f.mgmt, mgmtScope); err != nil {
		t.Fatalf("scoped create on own plane: %v", err)
	}

	t.Run("a scoped writer cannot touch a global target", func(t *testing.T) {
		_, err := s.UpsertExternalTarget(ctx, "svc", "10.0.0.1", 80, "", &f.mgmt, mgmtScope)
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("upsert global target as tenant = %v, want ErrNotFound", err)
		}
		if err := s.DeleteTarget(ctx, "svc", mgmtScope); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("delete global target as tenant = %v, want ErrNotFound", err)
		}
		// And it is genuinely still there for the operator.
		targets, err := s.ListTargets(ctx, nil)
		if err != nil {
			t.Fatalf("ListTargets: %v", err)
		}
		if !slices.ContainsFunc(targets, func(x store.TargetInfo) bool {
			return x.Name == "svc" && x.Address == "203.0.113.7"
		}) {
			t.Error("the global target was modified or removed by a scoped write")
		}
	})

	t.Run("a co-tenant's target is indistinguishable from a missing one", func(t *testing.T) {
		_, unknownErr := s.UpsertExternalTarget(ctx, "no-such-target", "10.0.0.9", 80, "", &f.defaultNet, defScope)
		if unknownErr != nil {
			t.Fatalf("creating a fresh name should succeed: %v", unknownErr)
		}
		foreign := s.DeleteTarget(ctx, "mgmt-svc", defScope)
		missing := s.DeleteTarget(ctx, "definitely-absent", defScope)
		if !errors.Is(foreign, store.ErrNotFound) {
			t.Fatalf("delete co-tenant target = %v, want ErrNotFound", foreign)
		}
		// Byte-identical bar the name: same sentence, same sentinel. If these
		// ever diverge, the 404 becomes an existence oracle.
		normalize := func(err error) string {
			return strings.ReplaceAll(strings.ReplaceAll(err.Error(), "mgmt-svc", "X"), "definitely-absent", "X")
		}
		if normalize(foreign) != normalize(missing) {
			t.Errorf("foreign %q and missing %q must be indistinguishable", foreign, missing)
		}
	})

	t.Run("ownership is immutable", func(t *testing.T) {
		_, err := s.UpsertExternalTarget(ctx, "mgmt-svc", "203.0.113.20", 443, "", &f.defaultNet, nil)
		if !errors.Is(err, store.ErrConflict) {
			t.Errorf("moving a target between planes = %v, want ErrConflict", err)
		}
	})

	t.Run("scoped listing shows global plus own plane, never a co-tenant's", func(t *testing.T) {
		targets, err := s.ListTargets(ctx, mgmtScope)
		if err != nil {
			t.Fatalf("ListTargets: %v", err)
		}
		var external []string
		for _, x := range targets {
			if x.Kind == "external" {
				external = append(external, x.Name)
			}
		}
		slices.Sort(external)
		// "svc" is global, "mgmt-svc" is ours; "no-such-target" belongs to
		// the default plane and must not appear.
		if !slices.Equal(external, []string{"mgmt-svc", "svc"}) {
			t.Errorf("scoped external targets = %v, want [mgmt-svc svc]", external)
		}
	})

	t.Run("an agent target cannot be given a plane of its own", func(t *testing.T) {
		// The plane of an agent target is the agent's; 0019's CHECK makes a
		// second, drifting copy non-representable.
		var got string
		err := s.Pool().QueryRow(ctx,
			`UPDATE targets SET network_id = $1 WHERE id = $2 RETURNING name`,
			f.mgmt, f.tADef).Scan(&got)
		if err == nil {
			t.Fatalf("agent target %q accepted a network_id", got)
		}
		if !strings.Contains(err.Error(), "targets_network_external_check") {
			t.Errorf("error = %v, want the targets_network_external_check violation", err)
		}
	})
}

func TestDeleteNetworkNamesBlockingTargets(t *testing.T) {
	t.Parallel()
	ctx, s := newStore(t)
	empty := createNetwork(t, ctx, s, "empty-plane")
	if _, err := s.UpsertExternalTarget(ctx, "tenant-svc", "203.0.113.30", 443, "", &empty, nil); err != nil {
		t.Fatalf("UpsertExternalTarget: %v", err)
	}
	// targets.network_id is ON DELETE RESTRICT. Without the blocker count
	// this surfaces as an opaque FK violation (a 500), not a refusal that
	// tells the operator what to clean up first.
	_, err := s.DeleteNetwork(ctx, "empty-plane")
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("DeleteNetwork with a tenant target = %v, want ErrConflict", err)
	}
	if !strings.Contains(err.Error(), "1 target(s)") {
		t.Errorf("conflict %q does not name the blocking target count", err)
	}
	if err := s.DeleteTarget(ctx, "tenant-svc", nil); err != nil {
		t.Fatalf("DeleteTarget: %v", err)
	}
	if _, err := s.DeleteNetwork(ctx, "empty-plane"); err != nil {
		t.Errorf("DeleteNetwork after clearing the target: %v", err)
	}
}

func TestPathThresholdPlanesAreIndependent(t *testing.T) {
	t.Parallel()
	ctx, s := newStore(t)
	f := buildNetFixture(t, ctx, s)

	warn := func(v int64) *int64 { return &v }
	// The all-planes row (pre-tenancy shape) and one plane-qualified row for
	// the SAME pair must coexist — this is what UNIQUE NULLS NOT DISTINCT on
	// (site_a_id, site_b_id, network_id) buys.
	if _, err := s.UpsertPathThreshold(ctx, "site-a", "site-b", nil,
		store.PathThresholdOverride{LatencyWarnUS: warn(1000), UpdatedBy: "op"}); err != nil {
		t.Fatalf("all-planes upsert: %v", err)
	}
	if _, err := s.UpsertPathThreshold(ctx, "site-a", "site-b", &f.mgmt,
		store.PathThresholdOverride{LatencyWarnUS: warn(2000), UpdatedBy: "tenant"}); err != nil {
		t.Fatalf("plane-qualified upsert: %v", err)
	}
	all, err := s.ListPathThresholds(ctx, nil)
	if err != nil {
		t.Fatalf("ListPathThresholds: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("rows = %d, want 2 (the all-planes row and the mgmt row)", len(all))
	}

	t.Run("re-upserting one plane updates only that row", func(t *testing.T) {
		if _, err := s.UpsertPathThreshold(ctx, "site-a", "site-b", &f.mgmt,
			store.PathThresholdOverride{LatencyWarnUS: warn(3000), UpdatedBy: "tenant"}); err != nil {
			t.Fatalf("re-upsert: %v", err)
		}
		rows, err := s.ListPathThresholds(ctx, nil)
		if err != nil {
			t.Fatalf("ListPathThresholds: %v", err)
		}
		if len(rows) != 2 {
			t.Fatalf("rows after re-upsert = %d, want 2 (ON CONFLICT must not insert a duplicate)", len(rows))
		}
		for _, r := range rows {
			want := int64(1000)
			if r.Network == "mgmt" {
				want = 3000
			}
			if r.LatencyWarnUS == nil || *r.LatencyWarnUS != want {
				t.Errorf("plane %q latency_warn_us = %v, want %d", r.Network, r.LatencyWarnUS, want)
			}
		}
	})

	t.Run("deleting one plane leaves the all-planes row", func(t *testing.T) {
		if err := s.DeletePathThreshold(ctx, "site-a", "site-b", &f.mgmt); err != nil {
			t.Fatalf("delete mgmt row: %v", err)
		}
		rows, err := s.ListPathThresholds(ctx, nil)
		if err != nil {
			t.Fatalf("ListPathThresholds: %v", err)
		}
		if len(rows) != 1 || rows[0].Network != "" {
			t.Errorf("rows after plane delete = %+v, want only the all-planes row", rows)
		}
		// And the all-planes row is addressed independently.
		if err := s.DeletePathThreshold(ctx, "site-a", "site-b", &f.mgmt); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("second delete = %v, want ErrNotFound", err)
		}
	})

	t.Run("a scoped reader sees its own plane and the all-planes row only", func(t *testing.T) {
		if _, err := s.UpsertPathThreshold(ctx, "site-a", "site-b", &f.defaultNet,
			store.PathThresholdOverride{LatencyWarnUS: warn(4000), UpdatedBy: "other"}); err != nil {
			t.Fatalf("co-tenant upsert: %v", err)
		}
		rows, err := s.ListPathThresholds(ctx, []uuid.UUID{f.mgmt})
		if err != nil {
			t.Fatalf("ListPathThresholds: %v", err)
		}
		for _, r := range rows {
			if r.Network != "" && r.Network != "mgmt" {
				t.Errorf("plane %q leaked into a mgmt-scoped read", r.Network)
			}
		}
		if len(rows) != 1 {
			t.Errorf("mgmt-scoped rows = %d, want 1 (the all-planes row; the default-plane row is hidden)", len(rows))
		}
	})
}

func TestPathThresholdPairsCarryPlane(t *testing.T) {
	t.Parallel()
	ctx, s := newStore(t)
	f := buildNetFixture(t, ctx, s)
	warn := int64(5000)
	if _, err := s.UpsertPathThreshold(ctx, "site-a", "site-b", &f.mgmt,
		store.PathThresholdOverride{LatencyWarnUS: &warn, UpdatedBy: "t"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// Ingest loads by SITE, both planes at once, and picks by the measuring
	// agent's own network — so the plane must survive the round trip.
	pairs, err := s.PathThresholdPairs(ctx, f.siteA)
	if err != nil {
		t.Fatalf("PathThresholdPairs: %v", err)
	}
	if len(pairs) != 1 {
		t.Fatalf("pairs = %d, want 1", len(pairs))
	}
	if pairs[0].NetworkID == nil || *pairs[0].NetworkID != f.mgmt {
		t.Errorf("NetworkID = %v, want %v", pairs[0].NetworkID, f.mgmt)
	}
}

func TestNetworkThresholdsScopedWrites(t *testing.T) {
	t.Parallel()
	ctx, s := newStore(t)
	f := buildNetFixture(t, ctx, s)
	mgmtScope := []uuid.UUID{f.mgmt}
	crit := 12.5

	if _, err := s.UpsertNetworkThreshold(ctx, "mgmt",
		store.NetworkThreshold{LossCritPct: &crit, UpdatedBy: "tenant"}, mgmtScope); err != nil {
		t.Fatalf("own-plane upsert: %v", err)
	}
	got, err := s.NetworkThresholdByID(ctx, f.mgmt)
	if err != nil || got == nil {
		t.Fatalf("NetworkThresholdByID = %v, %v", got, err)
	}
	if got.LossCritPct == nil || *got.LossCritPct != crit {
		t.Errorf("loss_crit_pct = %v, want %v", got.LossCritPct, crit)
	}
	if got.LatencyWarnUS != nil {
		t.Error("unset metrics must stay nil so they inherit the global row")
	}

	t.Run("a plane outside scope is ErrNotFound, not a refusal that confirms it", func(t *testing.T) {
		_, err := s.UpsertNetworkThreshold(ctx, "default",
			store.NetworkThreshold{LossCritPct: &crit, UpdatedBy: "tenant"}, mgmtScope)
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("foreign-plane upsert = %v, want ErrNotFound", err)
		}
		unknown, err2 := s.UpsertNetworkThreshold(ctx, "no-such-plane",
			store.NetworkThreshold{LossCritPct: &crit, UpdatedBy: "tenant"}, mgmtScope)
		if !errors.Is(err2, store.ErrNotFound) {
			t.Fatalf("unknown-plane upsert = %v (%v), want ErrNotFound", err2, unknown)
		}
		if strings.ReplaceAll(err.Error(), "default", "X") != strings.ReplaceAll(err2.Error(), "no-such-plane", "X") {
			t.Errorf("foreign %q and unknown %q must be indistinguishable", err, err2)
		}
		if err := s.DeleteNetworkThreshold(ctx, "default", mgmtScope); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("foreign-plane delete = %v, want ErrNotFound", err)
		}
	})

	t.Run("no overlay is nil, not an error", func(t *testing.T) {
		none, err := s.NetworkThresholdByID(ctx, f.defaultNet)
		if err != nil {
			t.Fatalf("NetworkThresholdByID: %v", err)
		}
		if none != nil {
			t.Errorf("got %+v, want nil — ingest treats nil as 'no layer'", none)
		}
	})

	t.Run("deleting the plane cascades the overlay", func(t *testing.T) {
		// network_thresholds is presentation config: it dies with its plane
		// and must NOT be a DeleteNetwork blocker.
		id := createNetwork(t, ctx, s, "throwaway")
		if _, err := s.UpsertNetworkThreshold(ctx, "throwaway",
			store.NetworkThreshold{LossCritPct: &crit, UpdatedBy: "op"}, nil); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		if _, err := s.DeleteNetwork(ctx, "throwaway"); err != nil {
			t.Fatalf("DeleteNetwork: %v", err)
		}
		gone, err := s.NetworkThresholdByID(ctx, id)
		if err != nil {
			t.Fatalf("NetworkThresholdByID: %v", err)
		}
		if gone != nil {
			t.Errorf("overlay survived its network: %+v", gone)
		}
	})
}

func TestJoinTokensScopedByPlane(t *testing.T) {
	t.Parallel()
	ctx, s := newStore(t)
	f := buildNetFixture(t, ctx, s)
	mgmtScope := []uuid.UUID{f.mgmt}

	if _, err := s.CreateJoinToken(ctx, f.siteA, f.mgmt, "tenant", time.Hour); err != nil {
		t.Fatalf("CreateJoinToken mgmt: %v", err)
	}
	if _, err := s.CreateJoinToken(ctx, f.siteA, f.defaultNet, "op", time.Hour); err != nil {
		t.Fatalf("CreateJoinToken default: %v", err)
	}
	scoped, err := s.ListJoinTokens(ctx, mgmtScope)
	if err != nil {
		t.Fatalf("ListJoinTokens: %v", err)
	}
	// A tenant admin that can mint tokens must be able to see its own — and
	// only its own; another plane's pending enrollment is not its business.
	// buildNetFixture's enrollments leave USED mgmt tokens behind (they are
	// the audit trail), so assert on the plane of every row rather than a
	// count that would silently track fixture churn.
	if len(scoped) == 0 {
		t.Fatal("scoped token list is empty; the tenant cannot see the tokens it may mint")
	}
	for _, tok := range scoped {
		if tok.Network != "mgmt" {
			t.Errorf("token %s on plane %q leaked into a mgmt-scoped list", tok.ID, tok.Network)
		}
	}
	all, err := s.ListJoinTokens(ctx, nil)
	if err != nil {
		t.Fatalf("ListJoinTokens nil: %v", err)
	}
	var foreign uuid.UUID
	for _, tok := range all {
		if tok.Network != "mgmt" {
			foreign = tok.ID
		}
	}
	if foreign == uuid.Nil {
		t.Fatal("fixture produced no off-plane token to test against")
	}
	if len(all) <= len(scoped) {
		t.Errorf("unscoped list (%d) must be larger than the mgmt-scoped one (%d)", len(all), len(scoped))
	}
	if err := s.DeleteJoinToken(ctx, foreign, mgmtScope); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleting a co-tenant's token = %v, want ErrNotFound", err)
	}
	// Deleting our own UNUSED token succeeds; the used ones are refused as
	// audit records, which is pre-existing behavior and not what this pins.
	var ours uuid.UUID
	for _, tok := range scoped {
		if tok.UsedAt == nil {
			ours = tok.ID
		}
	}
	if err := s.DeleteJoinToken(ctx, ours, mgmtScope); err != nil {
		t.Errorf("deleting own token: %v", err)
	}
}

// TestPathThresholdPairsStillIndexScans guards the 0020 PK swap. Dropping
// path_thresholds_pkey dropped the index that served PathThresholdPairs'
// site_a_id arm; the replacement UNIQUE (site_a_id, site_b_id, network_id)
// leads with the same column and substitutes for it, but nothing in the
// schema says so out loud. Ingest runs this query per agent per cache
// refresh, so a silent fall back to a sequential scan is a fleet-wide cost
// that no functional test would notice.
// Deliberately serial: an EXPLAIN-based planner assertion — concurrent load
// and autovacuum on sibling databases can shift plan choices.
func TestPathThresholdPairsStillIndexScans(t *testing.T) {
	ctx, s := newStore(t)
	f := buildNetFixture(t, ctx, s)
	// The planner picks a seq scan on a tiny table whatever the indexes, so
	// disable it: the question here is whether an index is USABLE at all,
	// not which one the planner prefers at fixture scale.
	if _, err := s.Pool().Exec(ctx, `SET enable_seqscan = off`); err != nil {
		t.Fatalf("disable seqscan: %v", err)
	}
	rows, err := s.Pool().Query(ctx, `
		EXPLAIN SELECT site_a_id, site_b_id, network_id,
		       latency_warn_us, latency_crit_us, loss_warn_pct, loss_crit_pct
		  FROM path_thresholds
		 WHERE site_a_id = $1 OR site_b_id = $1`, f.siteA)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan: %v", err)
		}
		plan = append(plan, line)
	}
	joined := strings.Join(plan, "\n")
	if strings.Contains(joined, "Seq Scan") {
		t.Errorf("PathThresholdPairs fell back to a Seq Scan:\n%s", joined)
	}
	// One index per arm of the OR.
	for _, want := range []string{"path_thresholds_pair_network_key", "path_thresholds_b_idx"} {
		if !strings.Contains(joined, want) {
			t.Errorf("plan does not use %s:\n%s", want, joined)
		}
	}
}

// TestDirectProbeCannotTargetACoTenantsTarget closes the gap between "the
// probe's network is mine" and "the thing it points at is mine to point at".
//
// Without the target check a tenant could attach a probe to a co-tenant's
// target row. That is not merely an existence oracle: the victim's own
// ListTargets probe_count is scope-filtered, so the foreign probe is
// INVISIBLE to them, yet DeleteTarget's unscoped count still refuses the
// delete — one tenant could pin another's target undeletable and the victim
// would see a target reporting zero probes that will not go away.
func TestDirectProbeCannotTargetACoTenantsTarget(t *testing.T) {
	t.Parallel()
	ctx, s := newStore(t)
	f := buildNetFixture(t, ctx, s)
	mgmtScope := []uuid.UUID{f.mgmt}

	// A target owned by the DEFAULT plane; our caller is scoped to mgmt.
	if _, err := s.UpsertExternalTarget(ctx, "their-svc", "203.0.113.40", 443, "", &f.defaultNet, nil); err != nil {
		t.Fatalf("UpsertExternalTarget: %v", err)
	}
	_, err := s.AddDirectProbe(ctx, "site-a", "their-svc", f.mgmt, netProbeSettings, true, "tenant", mgmtScope)
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("probing a co-tenant's target = %v, want ErrNotFound", err)
	}

	// A GLOBAL target stays probeable by everyone — that is what "global"
	// means, and the operator publishes them for exactly this.
	if _, err := s.AddDirectProbe(ctx, "site-a", "svc", f.mgmt, netProbeSettings, true, "tenant", mgmtScope); err != nil {
		t.Errorf("probing a global target: %v", err)
	}
	// And our own plane's target, naturally.
	if _, err := s.UpsertExternalTarget(ctx, "our-svc", "203.0.113.41", 443, "", &f.mgmt, mgmtScope); err != nil {
		t.Fatalf("UpsertExternalTarget own: %v", err)
	}
	if _, err := s.AddDirectProbe(ctx, "site-a", "our-svc", f.mgmt, netProbeSettings, true, "tenant", mgmtScope); err != nil {
		t.Errorf("probing our own target: %v", err)
	}
}

// TestTargetEndpointsScopesExternalOwnership closes the read half of target
// ownership. Before 0019 external targets had no plane, so TargetEndpoints
// scoped only the agent kind; leaving it that way would let a scoped user
// who guesses (or is told) a co-tenant's target UUID read its name,
// address, port, and URL through /api/v1/targets/{id}.
func TestTargetEndpointsScopesExternalOwnership(t *testing.T) {
	t.Parallel()
	ctx, s := newStore(t)
	f := buildNetFixture(t, ctx, s)
	mgmtScope := []uuid.UUID{f.mgmt}

	theirs, err := s.UpsertExternalTarget(ctx, "their-svc", "203.0.113.50", 8443, "", &f.defaultNet, nil)
	if err != nil {
		t.Fatalf("UpsertExternalTarget theirs: %v", err)
	}
	ours, err := s.UpsertExternalTarget(ctx, "our-svc", "203.0.113.51", 8443, "", &f.mgmt, nil)
	if err != nil {
		t.Fatalf("UpsertExternalTarget ours: %v", err)
	}
	global, err := s.UpsertExternalTarget(ctx, "global-svc", "203.0.113.52", 8443, "", nil, nil)
	if err != nil {
		t.Fatalf("UpsertExternalTarget global: %v", err)
	}

	// A co-tenant's target resolves to nil — httpapi turns that into the
	// same 404 an unknown UUID gets.
	ep, err := s.TargetEndpoints(ctx, theirs, mgmtScope)
	if err != nil {
		t.Fatalf("TargetEndpoints theirs: %v", err)
	}
	if ep != nil {
		t.Errorf("co-tenant target leaked: %+v", ep)
	}
	// Our own and the operator's published one stay readable.
	for name, id := range map[string]uuid.UUID{"ours": ours, "global": global} {
		ep, err := s.TargetEndpoints(ctx, id, mgmtScope)
		if err != nil {
			t.Fatalf("TargetEndpoints %s: %v", name, err)
		}
		if ep == nil {
			t.Errorf("%s target should be visible to the tenant", name)
		}
	}
	// And the operator sees everything.
	if ep, err := s.TargetEndpoints(ctx, theirs, nil); err != nil || ep == nil {
		t.Errorf("unscoped TargetEndpoints = %v, %v; want the row", ep, err)
	}
}

// TestMeshWritesAuthorizeUnderTheRowLock pins that mesh mutations refuse a
// foreign plane in the STORE, not merely in the handler.
//
// Mesh names are globally unique but reusable, so a handler-side
// name→network check can be invalidated by a delete-and-recreate on another
// plane before the store re-resolves the same name. Enforcing on the locked
// row removes that window; these calls prove the refusal exists at the layer
// that actually mutates.
func TestMeshWritesAuthorizeUnderTheRowLock(t *testing.T) {
	t.Parallel()
	ctx, s := newStore(t)
	f := buildNetFixture(t, ctx, s)
	mgmtScope := []uuid.UUID{f.mgmt}

	// buildNetFixture's "m1" sits on the default plane; our caller is mgmt.
	if err := s.AddMeshMember(ctx, "m1", "site-a", mgmtScope); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("AddMeshMember on a foreign mesh = %v, want ErrNotFound", err)
	}
	if err := s.RemoveMeshMember(ctx, "m1", "site-a", mgmtScope); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("RemoveMeshMember on a foreign mesh = %v, want ErrNotFound", err)
	}
	if _, err := s.AddMeshProbe(ctx, "m1", netProbeSettings, true, "tenant", mgmtScope); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("AddMeshProbe on a foreign mesh = %v, want ErrNotFound", err)
	}
	if _, err := s.DeleteMeshGroup(ctx, "m1", mgmtScope); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("DeleteMeshGroup on a foreign mesh = %v, want ErrNotFound", err)
	}
	// Indistinguishable from a mesh that does not exist.
	_, missing := s.DeleteMeshGroup(ctx, "no-such-mesh", mgmtScope)
	_, foreign := s.DeleteMeshGroup(ctx, "m1", mgmtScope)
	if strings.ReplaceAll(foreign.Error(), "m1", "X") != strings.ReplaceAll(missing.Error(), "no-such-mesh", "X") {
		t.Errorf("foreign %q and missing %q must be indistinguishable", foreign, missing)
	}

	// Our own plane's mesh works normally.
	if _, err := s.UpsertMeshGroup(ctx, "ours", &f.mgmt); err != nil {
		t.Fatalf("UpsertMeshGroup: %v", err)
	}
	if err := s.AddMeshMember(ctx, "ours", "site-a", mgmtScope); err != nil {
		t.Errorf("AddMeshMember on our own mesh: %v", err)
	}
	if _, err := s.DeleteMeshGroup(ctx, "ours", mgmtScope); err != nil {
		t.Errorf("DeleteMeshGroup on our own mesh: %v", err)
	}
	// And the operator is unaffected by any of it.
	if err := s.AddMeshMember(ctx, "m1", "site-a", nil); err != nil {
		t.Errorf("unscoped AddMeshMember: %v", err)
	}
}
