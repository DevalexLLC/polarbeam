package store_test

// DB-backed tenant-scoping tests, gated on POLARBEAM_TEST_DB_URL (see
// internal/server/dbtest). These pin the SQL that keeps one tenant's rows
// out of another tenant's reads — the network-scope predicates added for
// the network_admin/network_viewer roles — plus the user_networks lifecycle
// (create, replace, OIDC re-derivation, network-delete cascade) and the
// role CHECK widening of migration 0018.

import (
	"context"
	"crypto/sha256"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/devalexllc/polarbeam/internal/server/store"
)

// scopedReadFixture is buildNetFixture plus a site staffed ONLY on the
// default plane — the site a mgmt-scoped reader must never see.
func scopedReadFixture(t *testing.T, ctx context.Context, s *store.Store) (netFixture, uuid.UUID) {
	t.Helper()
	f := buildNetFixture(t, ctx, s)
	defOnly := enrollNetAgent(t, ctx, s, "site-def-only", "c-def", nil)
	return f, defOnly
}

func TestScopedReadsKeepPlanesApart(t *testing.T) {
	ctx, s := newStore(t)
	f, _ := scopedReadFixture(t, ctx, s)
	mgmtScope := []uuid.UUID{f.mgmt}

	t.Run("ListSites hides sites with no in-scope agents", func(t *testing.T) {
		sites, err := s.ListSites(ctx, mgmtScope)
		if err != nil {
			t.Fatalf("ListSites: %v", err)
		}
		var names []string
		for _, si := range sites {
			names = append(names, si.Name)
		}
		if !slices.Equal(names, []string{"site-a", "site-b"}) {
			t.Errorf("scoped sites = %v, want [site-a site-b] (site-def-only hidden)", names)
		}
		all, err := s.ListSites(ctx, nil)
		if err != nil {
			t.Fatalf("ListSites nil scope: %v", err)
		}
		if len(all) != 3 {
			t.Errorf("unscoped sites = %d, want 3", len(all))
		}
	})

	t.Run("ListAgents filters by plane", func(t *testing.T) {
		agents, err := s.ListAgents(ctx, mgmtScope)
		if err != nil {
			t.Fatalf("ListAgents: %v", err)
		}
		for _, a := range agents {
			if a.Network != "mgmt" {
				t.Errorf("scoped agent %s on plane %q leaked", a.Hostname, a.Network)
			}
		}
		if len(agents) != 2 {
			t.Errorf("scoped agents = %d, want 2", len(agents))
		}
	})

	t.Run("SiteEndpoints filters and 404s invisible sites", func(t *testing.T) {
		ep, err := s.SiteEndpoints(ctx, "site-a", mgmtScope)
		if err != nil {
			t.Fatalf("SiteEndpoints: %v", err)
		}
		if ep == nil || len(ep.AgentIDs) != 1 || ep.AgentIDs[0] != f.aMgmt {
			t.Errorf("scoped endpoints = %+v, want exactly the mgmt agent", ep)
		}
		invisible, err := s.SiteEndpoints(ctx, "site-def-only", mgmtScope)
		if err != nil {
			t.Fatalf("SiteEndpoints invisible: %v", err)
		}
		if invisible != nil {
			t.Errorf("site with no in-scope agents = %+v, want nil (unknown-site shape)", invisible)
		}
	})

	t.Run("ExpectedPairs filters by plane", func(t *testing.T) {
		// The fixture's mesh and direct probe are both on default: a mgmt
		// scope must see no expected pairs at all.
		pairs, err := s.ExpectedPairs(ctx, mgmtScope)
		if err != nil {
			t.Fatalf("ExpectedPairs: %v", err)
		}
		if len(pairs) != 0 {
			t.Errorf("mgmt-scoped expected pairs = %v, want none", pairs)
		}
		defPairs, err := s.ExpectedPairs(ctx, []uuid.UUID{f.defaultNet})
		if err != nil {
			t.Fatalf("ExpectedPairs default: %v", err)
		}
		if len(defPairs) == 0 {
			t.Error("default-scoped expected pairs empty, want the mesh pairs")
		}
	})

	t.Run("MatrixLatest filters by source plane", func(t *testing.T) {
		for _, row := range []struct {
			agent, target uuid.UUID
		}{{f.aDef, f.tBDef}, {f.aMgmt, f.tBMgmt}} {
			insertResults(t, ctx, s, row.agent, []store.ResultRow{{
				Time: time.Now().UTC().Truncate(time.Microsecond), TargetID: row.target,
				ProbeID: uuid.New(), ProbeType: 1, Status: 1, Sent: 1, Received: 1,
			}})
		}
		rows, err := s.MatrixLatest(ctx, time.Hour, mgmtScope)
		if err != nil {
			t.Fatalf("MatrixLatest: %v", err)
		}
		if len(rows) != 1 || rows[0].Network != "mgmt" {
			t.Errorf("scoped matrix rows = %+v, want exactly the mgmt series", rows)
		}
	})

	t.Run("target and config lists filter by plane", func(t *testing.T) {
		targets, err := s.ListTargets(ctx, mgmtScope)
		if err != nil {
			t.Fatalf("ListTargets: %v", err)
		}
		for _, tg := range targets {
			if tg.Kind == "agent" && tg.ID != f.tAMgmt && tg.ID != f.tBMgmt {
				t.Errorf("agent target %s (%s) leaked into mgmt scope", tg.Name, tg.ID)
			}
		}
		// external targets stay visible; agent targets shrink to the plane
		if n := len(targets); n != 3 { // 2 mgmt agent targets + 1 external
			t.Errorf("scoped targets = %d, want 3", n)
		}
		// The shared external target's probe count must describe the
		// CALLER's planes: the fixture's only probe against "svc" is on
		// default, so a mgmt-scoped caller must see 0 — a server-wide count
		// would leak another tenant's probe activity and contradict the
		// scoped probe list above.
		probeCountOf := func(list []store.TargetInfo, name string) int64 {
			t.Helper()
			for _, tg := range list {
				if tg.Name == name {
					return tg.ProbeCount
				}
			}
			t.Fatalf("target %q missing from %+v", name, list)
			return -1
		}
		if got := probeCountOf(targets, "svc"); got != 0 {
			t.Errorf("mgmt-scoped probe_count for shared external target = %d, want 0", got)
		}
		allTargets, err := s.ListTargets(ctx, nil)
		if err != nil {
			t.Fatalf("ListTargets nil scope: %v", err)
		}
		if got := probeCountOf(allTargets, "svc"); got != 1 {
			t.Errorf("unscoped probe_count for the external target = %d, want 1", got)
		}

		// Site reference counts are tenant activity too: a SHARED site
		// (agents on both planes) must not tell one tenant how large the
		// other's footprint there is.
		siteCfg := func(networks []uuid.UUID, name string) store.SiteAdminInfo {
			t.Helper()
			list, err := s.ListSitesConfig(ctx, networks)
			if err != nil {
				t.Fatalf("ListSitesConfig: %v", err)
			}
			for _, si := range list {
				if si.Name == name {
					return si
				}
			}
			t.Fatalf("site %q missing from %+v", name, list)
			return store.SiteAdminInfo{}
		}
		scoped := siteCfg(mgmtScope, "site-a")
		if scoped.AgentCount != 1 || scoped.MeshCount != 0 || scoped.ProbeCount != 0 {
			t.Errorf("mgmt-scoped site-a counts = (agents %d, meshes %d, probes %d), want (1, 0, 0)",
				scoped.AgentCount, scoped.MeshCount, scoped.ProbeCount)
		}
		global := siteCfg(nil, "site-a")
		if global.AgentCount != 2 || global.MeshCount != 1 || global.ProbeCount != 1 {
			t.Errorf("unscoped site-a counts = (agents %d, meshes %d, probes %d), want (2, 1, 1)",
				global.AgentCount, global.MeshCount, global.ProbeCount)
		}

		meshes, err := s.ListMeshGroups(ctx, mgmtScope)
		if err != nil {
			t.Fatalf("ListMeshGroups: %v", err)
		}
		if len(meshes) != 0 {
			t.Errorf("mgmt-scoped meshes = %v, want none (fixture mesh is default)", meshes)
		}

		probes, err := s.ListProbeConfigs(ctx, mgmtScope)
		if err != nil {
			t.Fatalf("ListProbeConfigs: %v", err)
		}
		if len(probes) != 0 {
			t.Errorf("mgmt-scoped probes = %d, want none", len(probes))
		}
		defProbes, err := s.ListProbeConfigs(ctx, []uuid.UUID{f.defaultNet})
		if err != nil {
			t.Fatalf("ListProbeConfigs default: %v", err)
		}
		if len(defProbes) != 2 { // mesh template (via mesh's plane) + direct
			t.Errorf("default-scoped probes = %d, want 2", len(defProbes))
		}

		nets, err := s.ListNetworksConfig(ctx, mgmtScope)
		if err != nil {
			t.Fatalf("ListNetworksConfig: %v", err)
		}
		if len(nets) != 1 || nets[0].Name != "mgmt" {
			t.Errorf("scoped networks = %+v, want exactly mgmt", nets)
		}
	})

	t.Run("events filter by plane and fail closed on deleted agents", func(t *testing.T) {
		orphan := uuid.New()
		for _, agent := range []uuid.UUID{f.aDef, f.aMgmt, orphan} {
			if _, err := s.Pool().Exec(ctx,
				`INSERT INTO outage_events (kind, agent_id, opened_at)
				 VALUES ('agent_offline', $1, now())`, agent); err != nil {
				t.Fatalf("insert outage: %v", err)
			}
			if _, err := s.Pool().Exec(ctx,
				`INSERT INTO path_events (time, agent_id, probe_id, target_id,
					old_path_hash, new_path_hash, old_hops, new_hops)
				 VALUES (now(), $1, gen_random_uuid(), $2, '\x01', '\x02', '[]', '[]')`,
				agent, f.tADef); err != nil {
				t.Fatalf("insert path event: %v", err)
			}
		}
		outages, _, err := s.ListOutages(ctx, 24*time.Hour, mgmtScope, false)
		if err != nil {
			t.Fatalf("ListOutages: %v", err)
		}
		if len(outages) != 1 || outages[0].AgentHostname != "a-mgmt" {
			t.Errorf("scoped outages = %+v, want exactly a-mgmt's (orphan excluded)", outages)
		}
		events, err := s.ListPathEvents(ctx, 24*time.Hour, mgmtScope)
		if err != nil {
			t.Fatalf("ListPathEvents: %v", err)
		}
		if len(events) != 1 || events[0].AgentHostname != "a-mgmt" {
			t.Errorf("scoped path events = %+v, want exactly a-mgmt's", events)
		}
	})

	t.Run("closed-outage cap counts only in-scope events", func(t *testing.T) {
		// 600 newer foreign closed outages would win a pre-filter global
		// LIMIT 500 and crowd the tenant's own (older) closed event out of
		// the response entirely; the scope predicate must run inside the
		// capped branch.
		if _, err := s.Pool().Exec(ctx, `
			INSERT INTO outage_events (kind, agent_id, opened_at, closed_at)
			SELECT 'agent_offline', $1, now() - interval '1 minute', now()
			  FROM generate_series(1, 600)`, f.aDef); err != nil {
			t.Fatalf("insert foreign closed outages: %v", err)
		}
		if _, err := s.Pool().Exec(ctx, `
			INSERT INTO outage_events (kind, agent_id, opened_at, closed_at)
			VALUES ('agent_offline', $1, now() - interval '2 hours', now())`, f.aMgmt); err != nil {
			t.Fatalf("insert own closed outage: %v", err)
		}
		outages, _, err := s.ListOutages(ctx, 24*time.Hour, mgmtScope, false)
		if err != nil {
			t.Fatalf("ListOutages: %v", err)
		}
		found := false
		for _, o := range outages {
			if o.Network != "mgmt" {
				t.Fatalf("foreign outage leaked: %+v", o)
			}
			if o.AgentHostname == "a-mgmt" && o.ClosedAt != nil {
				found = true
			}
		}
		if !found {
			t.Error("tenant's closed outage missing — the foreign volume consumed the cap")
		}
	})

	t.Run("path thresholds require both sites visible", func(t *testing.T) {
		if _, err := s.UpsertPathThreshold(ctx, "site-a", "site-b", nil,
			store.PathThresholdOverride{LatencyWarnUS: ptr(int64(1000)), UpdatedBy: "test"}); err != nil {
			t.Fatalf("UpsertPathThreshold a/b: %v", err)
		}
		if _, err := s.UpsertPathThreshold(ctx, "site-a", "site-def-only", nil,
			store.PathThresholdOverride{LatencyWarnUS: ptr(int64(2000)), UpdatedBy: "test"}); err != nil {
			t.Fatalf("UpsertPathThreshold a/def-only: %v", err)
		}
		overrides, err := s.ListPathThresholds(ctx, mgmtScope)
		if err != nil {
			t.Fatalf("ListPathThresholds: %v", err)
		}
		if len(overrides) != 1 || overrides[0].A != "site-a" || overrides[0].B != "site-b" {
			t.Errorf("scoped overrides = %+v, want only the site-a/site-b pair", overrides)
		}
		all, err := s.ListPathThresholds(ctx, nil)
		if err != nil {
			t.Fatalf("ListPathThresholds nil: %v", err)
		}
		if len(all) != 2 {
			t.Errorf("unscoped overrides = %d, want 2", len(all))
		}
	})

	t.Run("target endpoints hide foreign agent targets", func(t *testing.T) {
		ep, err := s.TargetEndpoints(ctx, f.tADef, mgmtScope)
		if err != nil {
			t.Fatalf("TargetEndpoints: %v", err)
		}
		if ep != nil {
			t.Errorf("foreign-plane agent target = %+v, want nil (unknown-target shape)", ep)
		}
		own, err := s.TargetEndpoints(ctx, f.tAMgmt, mgmtScope)
		if err != nil {
			t.Fatalf("TargetEndpoints own: %v", err)
		}
		if own == nil {
			t.Error("own-plane agent target invisible, want visible")
		}
	})
}

func ptr[T any](v T) *T { return &v }

// TestScopedSitesCoverExpectedPairs: ExpectedPairs deliberately keeps a
// configured pair whose member site has no agents (it renders as stale), so
// a scoped caller's site list and SiteEndpoints must cover that unstaffed
// site too — otherwise the scoped matrix references sites absent from
// /sites and pair detail 404s.
func TestScopedSitesCoverExpectedPairs(t *testing.T) {
	ctx, s := newStore(t)
	mgmt := createNetwork(t, ctx, s, "mgmt")
	enrollNetAgent(t, ctx, s, "site-a", "a-mgmt", &mgmt)
	if _, err := s.EnsureSite(ctx, "site-unstaffed"); err != nil {
		t.Fatalf("EnsureSite: %v", err)
	}
	if _, err := s.UpsertMeshGroup(ctx, "mgmt-mesh", &mgmt); err != nil {
		t.Fatalf("UpsertMeshGroup: %v", err)
	}
	for _, site := range []string{"site-a", "site-unstaffed"} {
		if err := s.AddMeshMember(ctx, "mgmt-mesh", site, nil); err != nil {
			t.Fatalf("AddMeshMember %q: %v", site, err)
		}
	}
	if _, err := s.AddMeshProbe(ctx, "mgmt-mesh", netProbeSettings, true, "test", nil); err != nil {
		t.Fatalf("AddMeshProbe: %v", err)
	}
	scope := []uuid.UUID{mgmt}

	pairs, err := s.ExpectedPairs(ctx, scope)
	if err != nil {
		t.Fatalf("ExpectedPairs: %v", err)
	}
	if len(pairs) == 0 {
		t.Fatal("expected pairs empty — fixture broken")
	}
	sites, err := s.ListSites(ctx, scope)
	if err != nil {
		t.Fatalf("ListSites: %v", err)
	}
	visible := map[string]bool{}
	for _, si := range sites {
		visible[si.Name] = true
	}
	for _, p := range pairs {
		if !visible[p.Src] || !visible[p.Dst] {
			t.Errorf("expected pair %s→%s references a site missing from the scoped site list %v", p.Src, p.Dst, sites)
		}
	}
	ep, err := s.SiteEndpoints(ctx, "site-unstaffed", scope)
	if err != nil {
		t.Fatalf("SiteEndpoints: %v", err)
	}
	if ep == nil {
		t.Error("unstaffed mesh-member site resolves to nil for its tenant — pair detail would 404")
	} else if len(ep.AgentIDs) != 0 {
		t.Errorf("unstaffed site endpoints = %+v, want empty sets (renders stale)", ep)
	}
}

// sessionFor inserts a raw session row for a user and loads it back — the
// scope-loading contract of GetSessionByTokenHash without the login dance.
func sessionFor(t *testing.T, ctx context.Context, s *store.Store, userID uuid.UUID) *store.SessionInfo {
	t.Helper()
	tokenHash := sha256.Sum256([]byte(userID.String()))
	if _, err := s.Pool().Exec(ctx, `
		INSERT INTO sessions (token_hash, user_id, csrf_token, expires_at)
		VALUES ($1, $2, 'csrf', now() + interval '1 hour')
		ON CONFLICT (token_hash) DO NOTHING`, tokenHash[:], userID); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	si, err := s.GetSessionByTokenHash(ctx, tokenHash[:])
	if err != nil {
		t.Fatalf("GetSessionByTokenHash: %v", err)
	}
	if si == nil {
		t.Fatal("session vanished")
	}
	return si
}

func TestScopedUserLifecycle(t *testing.T) {
	ctx, s := newStore(t)
	tenantA := createNetwork(t, ctx, s, "tenant-a")
	tenantB := createNetwork(t, ctx, s, "tenant-b")

	t.Run("create validation", func(t *testing.T) {
		if _, err := s.CreateUser(ctx, "bad1", "$argon2id$h", store.RoleNetworkAdmin, nil); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("scoped role without networks: err = %v, want ErrInvalid", err)
		}
		if _, err := s.CreateUser(ctx, "bad2", "$argon2id$h", store.RoleViewer, []uuid.UUID{tenantA}); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("global role with networks: err = %v, want ErrInvalid", err)
		}
		if _, err := s.CreateUser(ctx, "bad3", "$argon2id$h", "superuser", nil); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("unknown role: err = %v, want ErrInvalid (0018 CHECK never reached)", err)
		}
	})

	id, err := s.CreateUser(ctx, "tenant-admin", "$argon2id$h", store.RoleNetworkAdmin, []uuid.UUID{tenantB, tenantA})
	if err != nil {
		t.Fatalf("CreateUser scoped: %v", err)
	}

	t.Run("user and session carry sorted scope", func(t *testing.T) {
		u, err := s.GetUserByUsername(ctx, "tenant-admin")
		if err != nil || u == nil {
			t.Fatalf("GetUserByUsername: %v %v", u, err)
		}
		if !slices.Equal(u.Networks, []string{"tenant-a", "tenant-b"}) {
			t.Errorf("user networks = %v, want sorted [tenant-a tenant-b]", u.Networks)
		}
		si := sessionFor(t, ctx, s, id)
		scope, scoped := si.NetworkScope()
		if !scoped || len(scope) != 2 || scope[0].Name != "tenant-a" || scope[1].Name != "tenant-b" {
			t.Errorf("session scope = %v (scoped=%v), want both tenants", scope, scoped)
		}
	})

	t.Run("SetUserNetworks replaces and guards", func(t *testing.T) {
		if err := s.SetUserNetworks(ctx, id, []uuid.UUID{tenantA}); err != nil {
			t.Fatalf("SetUserNetworks: %v", err)
		}
		u, _ := s.GetUserByUsername(ctx, "tenant-admin")
		if !slices.Equal(u.Networks, []string{"tenant-a"}) {
			t.Errorf("replaced networks = %v, want [tenant-a]", u.Networks)
		}
		adminID, err := s.CreateUser(ctx, "global-admin", "$argon2id$h", store.RoleAdmin, nil)
		if err != nil {
			t.Fatalf("CreateUser admin: %v", err)
		}
		if err := s.SetUserNetworks(ctx, adminID, []uuid.UUID{tenantA}); !errors.Is(err, store.ErrConflict) {
			t.Errorf("scoping a global role: err = %v, want ErrConflict", err)
		}
	})

	t.Run("network admins never satisfy the last-admin guard", func(t *testing.T) {
		// global-admin (created above) is the ONLY enabled admin; the
		// network_admin must not count as one.
		var adminID uuid.UUID
		if err := s.Pool().QueryRow(ctx, `SELECT id FROM users WHERE username = 'global-admin'`).Scan(&adminID); err != nil {
			t.Fatalf("resolve admin: %v", err)
		}
		if err := s.SetUserDisabled(ctx, adminID, true); !errors.Is(err, store.ErrConflict) {
			t.Errorf("disabling the last global admin: err = %v, want ErrConflict", err)
		}
	})

	t.Run("RecordLogin passes the widened role CHECK", func(t *testing.T) {
		if err := s.RecordLogin(ctx, id); err != nil {
			t.Errorf("RecordLogin for network_admin: %v", err)
		}
	})

	t.Run("account list carries the scope", func(t *testing.T) {
		accounts, _, err := s.ListUserAccounts(ctx, store.UserAccountFilter{Role: store.RoleNetworkAdmin, Limit: 10})
		if err != nil {
			t.Fatalf("ListUserAccounts: %v", err)
		}
		if len(accounts) != 1 || !slices.Equal(accounts[0].Networks, []string{"tenant-a"}) {
			t.Errorf("accounts = %+v, want one network_admin over [tenant-a]", accounts)
		}
	})

	t.Run("deleting a network cascades scope away and fails closed", func(t *testing.T) {
		// tenant-a has no agents/meshes/probes/tokens — only the user's
		// scope row, which must never block the delete.
		if _, err := s.DeleteNetwork(ctx, "tenant-a"); err != nil {
			t.Fatalf("DeleteNetwork: %v", err)
		}
		si := sessionFor(t, ctx, s, id)
		scope, scoped := si.NetworkScope()
		if !scoped || len(scope) != 0 {
			t.Errorf("post-delete scope = %v (scoped=%v), want empty non-nil (sees nothing)", scope, scoped)
		}
	})
}

func TestUpsertOIDCUserScopeTracksIdP(t *testing.T) {
	ctx, s := newStore(t)
	tenantA := createNetwork(t, ctx, s, "tenant-a")
	tenantB := createNetwork(t, ctx, s, "tenant-b")
	issuer := "https://idp.example/realms/x"
	cur, err := s.GetOIDCSettings(ctx)
	if err != nil {
		t.Fatalf("GetOIDCSettings: %v", err)
	}
	policy := cur.UpdatedAt

	countScope := func(u *store.UserInfo) []string {
		got, err := s.GetUserByUsername(ctx, u.Username)
		if err != nil || got == nil {
			t.Fatalf("reload %q: %v %v", u.Username, got, err)
		}
		return got.Networks
	}

	u, err := s.UpsertOIDCUser(ctx, issuer, "sub-1", "tenant-user", store.RoleNetworkAdmin, []uuid.UUID{tenantA, tenantB}, policy)
	if err != nil {
		t.Fatalf("UpsertOIDCUser: %v", err)
	}
	if got := countScope(u); !slices.Equal(got, []string{"tenant-a", "tenant-b"}) {
		t.Fatalf("initial scope = %v", got)
	}

	// A group removed at the IdP shrinks the scope on the next login.
	if _, err := s.UpsertOIDCUser(ctx, issuer, "sub-1", "tenant-user", store.RoleNetworkAdmin, []uuid.UUID{tenantB}, policy); err != nil {
		t.Fatalf("UpsertOIDCUser shrink: %v", err)
	}
	if got := countScope(u); !slices.Equal(got, []string{"tenant-b"}) {
		t.Fatalf("shrunk scope = %v, want [tenant-b]", got)
	}

	// A promotion to a global role clears the scope entirely.
	if _, err := s.UpsertOIDCUser(ctx, issuer, "sub-1", "tenant-user", store.RoleAdmin, nil, policy); err != nil {
		t.Fatalf("UpsertOIDCUser promote: %v", err)
	}
	if got := countScope(u); got != nil {
		t.Fatalf("post-promotion scope = %v, want nil", got)
	}

	// A write mapped under a superseded settings revision is refused: the
	// stale role/scope must never overwrite what a newer policy (or its
	// session revocation) established.
	if _, err := s.UpsertOIDCUser(ctx, issuer, "sub-1", "tenant-user", store.RoleViewer, nil,
		policy.Add(-time.Second)); !errors.Is(err, store.ErrOIDCPolicyChanged) {
		t.Fatalf("stale-policy upsert: err = %v, want ErrOIDCPolicyChanged", err)
	}
	if got, _ := s.GetUserByUsername(ctx, "tenant-user"); got.Role != store.RoleAdmin {
		t.Errorf("role after refused stale upsert = %q, want admin (unchanged)", got.Role)
	}
}

// TestOIDCPolicyChangeRevokesSessions: rewriting the role mapping without
// touching the provider must sign every federated user out — sessions join
// role and scope live from the users row, which only a login remaps.
func TestOIDCPolicyChangeRevokesSessions(t *testing.T) {
	ctx, s := newStore(t)
	cur, err := s.GetOIDCSettings(ctx)
	if err != nil {
		t.Fatalf("GetOIDCSettings: %v", err)
	}
	u, err := s.UpsertOIDCUser(ctx, "https://idp.example/realms/x", "sub-1", "fed-viewer",
		store.RoleViewer, nil, cur.UpdatedAt)
	if err != nil {
		t.Fatalf("UpsertOIDCUser: %v", err)
	}
	si := sessionFor(t, ctx, s, u.ID)

	next := *cur
	next.UnmatchedRole = "deny"
	next.UpdatedBy = "test"
	_, revoked, err := s.UpdateOIDCSettings(ctx, next, true, false, false)
	if err != nil {
		t.Fatalf("UpdateOIDCSettings: %v", err)
	}
	if revoked != 1 {
		t.Errorf("revoked = %d, want 1 (the federated session)", revoked)
	}
	tokenHash := sha256.Sum256([]byte(u.ID.String()))
	gone, err := s.GetSessionByTokenHash(ctx, tokenHash[:])
	if err != nil {
		t.Fatalf("GetSessionByTokenHash: %v", err)
	}
	if gone != nil {
		t.Errorf("session %s survived the policy change", si.ID)
	}

	// An identical re-save changes nothing and revokes nothing.
	if _, revoked, err = s.UpdateOIDCSettings(ctx, next, true, false, false); err != nil || revoked != 0 {
		t.Errorf("no-op save: revoked = %d err = %v, want 0 revocations", revoked, err)
	}

	// role_claim is a claim→role mapping input too: changing it must revoke.
	sessionFor(t, ctx, s, u.ID)
	next.RoleClaim = "entitlements"
	if _, revoked, err = s.UpdateOIDCSettings(ctx, next, true, false, false); err != nil || revoked != 1 {
		t.Errorf("role_claim change: revoked = %d err = %v, want 1", revoked, err)
	}

	// The keep flags resolve against the LOCKED row: a keep-all save right
	// after is a no-op even though this caller's o carries stale policy
	// fields.
	stale := next
	stale.RoleRules = []store.OIDCRoleRule{{Value: "ghost", Role: store.RoleNetworkAdmin, Networks: []string{"x"}}}
	stale.UnmatchedRole = "viewer"
	out, revoked, err := s.UpdateOIDCSettings(ctx, stale, true, true, true)
	if err != nil || revoked != 0 {
		t.Fatalf("keep-all save: revoked = %d err = %v, want 0", revoked, err)
	}
	if out.UnmatchedRole != "deny" || len(out.RoleRules) != 0 {
		t.Errorf("keep-all save stored = %+v, want locked row's policy kept (deny, no rules)", out)
	}
}

func TestOIDCSettingsRoleRulesRoundTrip(t *testing.T) {
	ctx, s := newStore(t)
	rules := []store.OIDCRoleRule{
		{Value: "tenant-a-admins", Role: store.RoleNetworkAdmin, Networks: []string{"tenant-a"}},
		{Value: "tenant-a-viewers", Role: store.RoleNetworkViewer, Networks: []string{"tenant-a", "tenant-b"}},
	}
	in := store.OIDCSettings{
		Scopes: []string{"openid"}, UsernameClaim: "preferred_username", RoleClaim: "groups",
		AdminValues: []string{}, RoleRules: rules, UnmatchedRole: "deny", UpdatedBy: "test",
	}
	if _, _, err := s.UpdateOIDCSettings(ctx, in, false, false, false); err != nil {
		t.Fatalf("UpdateOIDCSettings: %v", err)
	}
	out, err := s.GetOIDCSettings(ctx)
	if err != nil {
		t.Fatalf("GetOIDCSettings: %v", err)
	}
	if out.UnmatchedRole != "deny" || len(out.RoleRules) != 2 ||
		out.RoleRules[0].Value != "tenant-a-admins" || out.RoleRules[0].Role != store.RoleNetworkAdmin ||
		!slices.Equal(out.RoleRules[1].Networks, []string{"tenant-a", "tenant-b"}) {
		t.Errorf("round-tripped settings = %+v", out)
	}
}
