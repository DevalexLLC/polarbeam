package store_test

// DB-backed network-scoping tests, gated on POLARBEAM_TEST_DB_URL (see
// internal/server/dbtest). These pin the SQL predicates that keep agents on
// separate network planes apart: enrollment inheriting the token's network,
// config-input scoping, the enabled-ID re-derivation, expected pairs, and
// mesh cleanup — none of which the pure meshexpand tests can exercise.

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/devalexllc/polarbeam/internal/server/meshexpand"
	"github.com/devalexllc/polarbeam/internal/server/probeid"
	"github.com/devalexllc/polarbeam/internal/server/store"
)

var certSerial atomic.Int64

func createNetwork(t *testing.T, ctx context.Context, s *store.Store, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := s.Pool().QueryRow(ctx,
		`INSERT INTO networks (name) VALUES ($1) RETURNING id`, name).Scan(&id); err != nil {
		t.Fatalf("insert network %q: %v", name, err)
	}
	return id
}

func networkIDByName(t *testing.T, ctx context.Context, s *store.Store, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := s.Pool().QueryRow(ctx,
		`SELECT id FROM networks WHERE name = $1`, name).Scan(&id); err != nil {
		t.Fatalf("resolve network %q: %v", name, err)
	}
	return id
}

// enrollNetAgent enrolls an agent through the real token flow. A non-nil
// network moves the token onto that network first via SQL — deliberately the
// only way to mint a non-default token until a --network surface exists.
func enrollNetAgent(t *testing.T, ctx context.Context, s *store.Store, siteName, hostname string, network *uuid.UUID) uuid.UUID {
	t.Helper()
	siteID, err := s.EnsureSite(ctx, siteName)
	if err != nil {
		t.Fatalf("EnsureSite %q: %v", siteName, err)
	}
	token, err := s.CreateJoinToken(ctx, siteID, "test", time.Hour)
	if err != nil {
		t.Fatalf("CreateJoinToken: %v", err)
	}
	if network != nil {
		tokenID, _, _ := strings.Cut(token, ".")
		if _, err := s.Pool().Exec(ctx,
			`UPDATE join_tokens SET network_id = $2 WHERE id = $1`, tokenID, *network); err != nil {
			t.Fatalf("move token to network: %v", err)
		}
	}
	agentID, _, err := s.EnrollAgent(ctx, token, hostname, hostname+":9443", "v0", []byte(hostname),
		func(uuid.UUID) (store.IssuedCert, error) {
			return store.IssuedCert{
				Serial:    big.NewInt(certSerial.Add(1)),
				NotBefore: time.Now().Add(-time.Hour),
				NotAfter:  time.Now().Add(time.Hour),
			}, nil
		})
	if err != nil {
		t.Fatalf("EnrollAgent %q: %v", hostname, err)
	}
	return agentID
}

func agentTargetID(t *testing.T, ctx context.Context, s *store.Store, agentID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := s.Pool().QueryRow(ctx,
		`SELECT id FROM targets WHERE agent_id = $1`, agentID).Scan(&id); err != nil {
		t.Fatalf("agent target for %s: %v", agentID, err)
	}
	return id
}

var netProbeSettings = store.ProbeSettings{
	ProbeType: 1, Interval: time.Minute, Timeout: 5 * time.Second, Params: map[string]string{},
}

// netFixture is two sites, each staffed with one default-network and one
// mgmt-network agent, a default mesh over both sites with one template, and
// a default direct probe at site-a against an external target.
type netFixture struct {
	defaultNet, mgmt             uuid.UUID
	siteA, siteB                 uuid.UUID
	aDef, aMgmt, bDef, bMgmt     uuid.UUID
	tADef, tAMgmt, tBDef, tBMgmt uuid.UUID // agent-kind target IDs
	tmplID, directID             uuid.UUID
}

func buildNetFixture(t *testing.T, ctx context.Context, s *store.Store) netFixture {
	t.Helper()
	var f netFixture
	f.defaultNet = networkIDByName(t, ctx, s, "default")
	f.mgmt = createNetwork(t, ctx, s, "mgmt")

	f.aDef = enrollNetAgent(t, ctx, s, "site-a", "a-def", nil)
	f.aMgmt = enrollNetAgent(t, ctx, s, "site-a", "a-mgmt", &f.mgmt)
	f.bDef = enrollNetAgent(t, ctx, s, "site-b", "b-def", nil)
	f.bMgmt = enrollNetAgent(t, ctx, s, "site-b", "b-mgmt", &f.mgmt)
	f.tADef = agentTargetID(t, ctx, s, f.aDef)
	f.tAMgmt = agentTargetID(t, ctx, s, f.aMgmt)
	f.tBDef = agentTargetID(t, ctx, s, f.bDef)
	f.tBMgmt = agentTargetID(t, ctx, s, f.bMgmt)

	var err error
	if f.siteA, err = s.SiteIDByName(ctx, "site-a"); err != nil {
		t.Fatalf("SiteIDByName: %v", err)
	}
	if f.siteB, err = s.SiteIDByName(ctx, "site-b"); err != nil {
		t.Fatalf("SiteIDByName: %v", err)
	}

	if _, err := s.UpsertMeshGroup(ctx, "m1"); err != nil {
		t.Fatalf("UpsertMeshGroup: %v", err)
	}
	for _, site := range []string{"site-a", "site-b"} {
		if err := s.AddMeshMember(ctx, "m1", site); err != nil {
			t.Fatalf("AddMeshMember %s: %v", site, err)
		}
	}
	if f.tmplID, err = s.AddMeshProbe(ctx, "m1", netProbeSettings, true, "test"); err != nil {
		t.Fatalf("AddMeshProbe: %v", err)
	}

	if _, err := s.UpsertExternalTarget(ctx, "svc", "203.0.113.7", 443, ""); err != nil {
		t.Fatalf("UpsertExternalTarget: %v", err)
	}
	if f.directID, err = s.AddDirectProbe(ctx, "site-a", "svc", netProbeSettings, true, "test"); err != nil {
		t.Fatalf("AddDirectProbe: %v", err)
	}
	return f
}

func moveMeshToNetwork(t *testing.T, ctx context.Context, s *store.Store, meshName string, network uuid.UUID) {
	t.Helper()
	if _, err := s.Pool().Exec(ctx,
		`UPDATE mesh_groups SET network_id = $2 WHERE name = $1`, meshName, network); err != nil {
		t.Fatalf("move mesh %q: %v", meshName, err)
	}
}

func snapshotProbeIDs(t *testing.T, ctx context.Context, s *store.Store, agentID uuid.UUID) map[string]bool {
	t.Helper()
	in, err := s.LoadAgentConfigInputs(ctx, agentID)
	if err != nil {
		t.Fatalf("LoadAgentConfigInputs %s: %v", agentID, err)
	}
	snap, err := meshexpand.BuildSnapshot(in)
	if err != nil {
		t.Fatalf("BuildSnapshot %s: %v", agentID, err)
	}
	out := make(map[string]bool, len(snap.Probes))
	for _, p := range snap.Probes {
		out[p.ProbeId] = true
	}
	return out
}

func TestEnrollAgentInheritsTokenNetwork(t *testing.T) {
	ctx, s := newStore(t)
	defaultNet := networkIDByName(t, ctx, s, "default")
	mgmt := createNetwork(t, ctx, s, "mgmt")

	networkOf := func(agentID uuid.UUID) uuid.UUID {
		var id uuid.UUID
		if err := s.Pool().QueryRow(ctx,
			`SELECT network_id FROM agents WHERE id = $1`, agentID).Scan(&id); err != nil {
			t.Fatalf("agent network: %v", err)
		}
		return id
	}

	// Freshly minted tokens land on the default network.
	siteID, err := s.EnsureSite(ctx, "site-a")
	if err != nil {
		t.Fatalf("EnsureSite: %v", err)
	}
	token, err := s.CreateJoinToken(ctx, siteID, "test", time.Hour)
	if err != nil {
		t.Fatalf("CreateJoinToken: %v", err)
	}
	tokenID, _, _ := strings.Cut(token, ".")
	var tokenNet uuid.UUID
	if err := s.Pool().QueryRow(ctx,
		`SELECT network_id FROM join_tokens WHERE id = $1`, tokenID).Scan(&tokenNet); err != nil {
		t.Fatalf("token network: %v", err)
	}
	if tokenNet != defaultNet {
		t.Errorf("new token network = %s, want default %s", tokenNet, defaultNet)
	}

	if got := networkOf(enrollNetAgent(t, ctx, s, "site-a", "a1", nil)); got != defaultNet {
		t.Errorf("default-token agent network = %s, want default %s", got, defaultNet)
	}
	if got := networkOf(enrollNetAgent(t, ctx, s, "site-a", "a2", &mgmt)); got != mgmt {
		t.Errorf("mgmt-token agent network = %s, want mgmt %s", got, mgmt)
	}
}

func TestLoadAgentConfigInputsScopedByNetwork(t *testing.T) {
	ctx, s := newStore(t)
	f := buildNetFixture(t, ctx, s)

	// Default agent at site-a: the direct probe plus the mesh probe toward
	// site-b's default agent only — never the mgmt peer.
	in, err := s.LoadAgentConfigInputs(ctx, f.aDef)
	if err != nil {
		t.Fatalf("LoadAgentConfigInputs aDef: %v", err)
	}
	if len(in.Direct) != 1 || in.Direct[0].ID != f.directID {
		t.Errorf("aDef direct = %+v, want the one direct probe %s", in.Direct, f.directID)
	}
	if len(in.Mesh) != 1 || in.Mesh[0].ConfigID != f.tmplID {
		t.Errorf("aDef mesh = %+v, want template %s", in.Mesh, f.tmplID)
	}
	if len(in.Peers) != 1 || in.Peers[0].AgentID != f.bDef || in.Peers[0].TargetID != f.tBDef {
		t.Errorf("aDef peers = %+v, want only b-def (target %s)", in.Peers, f.tBDef)
	}

	// Mgmt agent at the same site: no default mesh, no default direct probe.
	if ids := snapshotProbeIDs(t, ctx, s, f.aMgmt); len(ids) != 0 {
		t.Errorf("aMgmt snapshot probes = %v, want empty", ids)
	}

	// Move the mesh to mgmt: the mirror image.
	moveMeshToNetwork(t, ctx, s, "m1", f.mgmt)
	if ids := snapshotProbeIDs(t, ctx, s, f.aDef); len(ids) != 1 || !ids[f.directID.String()] {
		t.Errorf("aDef after flip = %v, want only direct %s", ids, f.directID)
	}
	in, err = s.LoadAgentConfigInputs(ctx, f.aMgmt)
	if err != nil {
		t.Fatalf("LoadAgentConfigInputs aMgmt: %v", err)
	}
	if len(in.Direct) != 0 {
		t.Errorf("aMgmt direct after flip = %+v, want none (direct probe stays default)", in.Direct)
	}
	if len(in.Mesh) != 1 || len(in.Peers) != 1 || in.Peers[0].TargetID != f.tBMgmt {
		t.Errorf("aMgmt after flip: mesh %+v peers %+v, want template + b-mgmt peer", in.Mesh, in.Peers)
	}
}

func TestEnabledProbeIDsMatchAgentSnapshots(t *testing.T) {
	ctx, s := newStore(t)
	f := buildNetFixture(t, ctx, s)

	got, err := s.EnabledProbeIDs(ctx)
	if err != nil {
		t.Fatalf("EnabledProbeIDs: %v", err)
	}
	gotSet := make(map[uuid.UUID]bool, len(got))
	for _, id := range got {
		gotSet[id] = true
	}

	// Exact equality: every member site is staffed on the mesh network, so
	// the derived set is precisely what the agents run — no cross-network
	// destination ever enters it.
	want := map[uuid.UUID]bool{
		f.directID: true,
		probeid.MeshProbeID(f.tmplID, f.siteA, f.tBDef): true,
		probeid.MeshProbeID(f.tmplID, f.siteB, f.tADef): true,
	}
	if len(gotSet) != len(want) {
		t.Errorf("EnabledProbeIDs = %v (%d ids), want %d", got, len(gotSet), len(want))
	}
	for id := range want {
		if !gotSet[id] {
			t.Errorf("EnabledProbeIDs missing %s", id)
		}
	}
	for _, forbidden := range []uuid.UUID{
		probeid.MeshProbeID(f.tmplID, f.siteA, f.tBMgmt),
		probeid.MeshProbeID(f.tmplID, f.siteB, f.tAMgmt),
	} {
		if gotSet[forbidden] {
			t.Errorf("EnabledProbeIDs contains cross-network ID %s", forbidden)
		}
	}

	// Parity with what agents actually receive.
	union := make(map[string]bool)
	for _, agent := range []uuid.UUID{f.aDef, f.aMgmt, f.bDef, f.bMgmt} {
		for id := range snapshotProbeIDs(t, ctx, s, agent) {
			union[id] = true
		}
	}
	for id := range union {
		if !gotSet[uuid.MustParse(id)] {
			t.Errorf("agent runs %s but EnabledProbeIDs omits it", id)
		}
	}
}

func TestExpectedPairsNetworkScoped(t *testing.T) {
	ctx, s := newStore(t)
	f := buildNetFixture(t, ctx, s)

	// site-c has agents, but none on the mesh's network: its pairs vanish.
	enrollNetAgent(t, ctx, s, "site-c", "c-mgmt", &f.mgmt)
	if err := s.AddMeshMember(ctx, "m1", "site-c"); err != nil {
		t.Fatalf("AddMeshMember site-c: %v", err)
	}
	// site-d has no agents at all: today's stale-cell behavior is preserved,
	// so its pairs stay.
	if _, err := s.EnsureSite(ctx, "site-d"); err != nil {
		t.Fatalf("EnsureSite site-d: %v", err)
	}
	if err := s.AddMeshMember(ctx, "m1", "site-d"); err != nil {
		t.Fatalf("AddMeshMember site-d: %v", err)
	}

	pairs, err := s.ExpectedPairs(ctx)
	if err != nil {
		t.Fatalf("ExpectedPairs: %v", err)
	}
	got := make(map[string]bool, len(pairs))
	for _, p := range pairs {
		got[p.Src+">"+p.Dst] = true
	}
	want := map[string]bool{
		"site-a>site-b": true, "site-b>site-a": true,
		"site-a>site-d": true, "site-d>site-a": true,
		"site-b>site-d": true, "site-d>site-b": true,
	}
	if len(got) != len(want) {
		t.Errorf("ExpectedPairs = %v, want exactly %v", got, want)
	}
	for p := range want {
		if !got[p] {
			t.Errorf("ExpectedPairs missing %s", p)
		}
	}
	for p := range got {
		if strings.Contains(p, "site-c") {
			t.Errorf("ExpectedPairs contains off-network site-c pair %s", p)
		}
	}
}

func seriesExists(t *testing.T, ctx context.Context, s *store.Store, probeID uuid.UUID) bool {
	t.Helper()
	var n int
	if err := s.Pool().QueryRow(ctx,
		`SELECT count(*) FROM series_state WHERE probe_id = $1`, probeID).Scan(&n); err != nil {
		t.Fatalf("count series_state: %v", err)
	}
	return n > 0
}

func TestDeleteMeshGroupCleansOnNetworkSeries(t *testing.T) {
	ctx, s := newStore(t)
	f := buildNetFixture(t, ctx, s)

	// The series default-network agents actually produce. The mgmt agents at
	// the same sites are the trap: an unscoped meshMemberTargets would still
	// derive the right IDs plus phantoms, but a wrongly-scoped one (e.g.
	// matching on the agent's own network) would miss these.
	idAB := probeid.MeshProbeID(f.tmplID, f.siteA, f.tBDef)
	idBA := probeid.MeshProbeID(f.tmplID, f.siteB, f.tADef)
	seedSeriesState(t, ctx, s, f.aDef, idAB, f.tBDef, 1)
	seedSeriesState(t, ctx, s, f.bDef, idBA, f.tADef, 1)

	if _, err := s.DeleteMeshGroup(ctx, "m1"); err != nil {
		t.Fatalf("DeleteMeshGroup: %v", err)
	}
	if seriesExists(t, ctx, s, idAB) || seriesExists(t, ctx, s, idBA) {
		t.Errorf("mesh series survived DeleteMeshGroup")
	}
}

func TestRemoveMeshMemberRetiresOnNetworkSeries(t *testing.T) {
	ctx, s := newStore(t)
	f := buildNetFixture(t, ctx, s)

	cDef := enrollNetAgent(t, ctx, s, "site-c", "c-def", nil)
	tCDef := agentTargetID(t, ctx, s, cDef)
	siteC, err := s.SiteIDByName(ctx, "site-c")
	if err != nil {
		t.Fatalf("SiteIDByName: %v", err)
	}
	if err := s.AddMeshMember(ctx, "m1", "site-c"); err != nil {
		t.Fatalf("AddMeshMember site-c: %v", err)
	}

	type series struct {
		agent, probe, target uuid.UUID
	}
	all := []series{
		{f.aDef, probeid.MeshProbeID(f.tmplID, f.siteA, f.tBDef), f.tBDef},
		{f.bDef, probeid.MeshProbeID(f.tmplID, f.siteB, f.tADef), f.tADef},
		{f.aDef, probeid.MeshProbeID(f.tmplID, f.siteA, tCDef), tCDef},
		{cDef, probeid.MeshProbeID(f.tmplID, siteC, f.tADef), f.tADef},
		{f.bDef, probeid.MeshProbeID(f.tmplID, f.siteB, tCDef), tCDef},
		{cDef, probeid.MeshProbeID(f.tmplID, siteC, f.tBDef), f.tBDef},
	}
	for _, sr := range all {
		seedSeriesState(t, ctx, s, sr.agent, sr.probe, sr.target, 1)
	}

	if err := s.RemoveMeshMember(ctx, "m1", "site-b"); err != nil {
		t.Fatalf("RemoveMeshMember: %v", err)
	}
	// site-b's outbound and inbound series retire; a↔c survives.
	for i, sr := range all {
		gone := i == 0 || i == 1 || i == 4 || i == 5
		if got := seriesExists(t, ctx, s, sr.probe); got == gone {
			t.Errorf("series %d (%s): exists = %v, want retired = %v", i, sr.probe, got, gone)
		}
	}
}

func TestUpsertMeshGroupKeepsExistingNetwork(t *testing.T) {
	ctx, s := newStore(t)
	mgmt := createNetwork(t, ctx, s, "mgmt")

	first, err := s.UpsertMeshGroup(ctx, "m1")
	if err != nil {
		t.Fatalf("UpsertMeshGroup: %v", err)
	}
	moveMeshToNetwork(t, ctx, s, "m1", mgmt)

	again, err := s.UpsertMeshGroup(ctx, "m1")
	if err != nil {
		t.Fatalf("UpsertMeshGroup again: %v", err)
	}
	if first != again {
		t.Errorf("upsert returned %s then %s for the same mesh", first, again)
	}
	var net uuid.UUID
	if err := s.Pool().QueryRow(ctx,
		`SELECT network_id FROM mesh_groups WHERE name = 'm1'`).Scan(&net); err != nil {
		t.Fatalf("mesh network: %v", err)
	}
	if net != mgmt {
		t.Errorf("re-upsert moved mesh to %s, want it kept on mgmt %s", net, mgmt)
	}
}

func TestNetworkCRUD(t *testing.T) {
	ctx, s := newStore(t)

	id, err := s.CreateNetwork(ctx, "mgmt", "Management")
	if err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	if got, err := s.NetworkIDByName(ctx, "mgmt"); err != nil || got != id {
		t.Errorf("NetworkIDByName(mgmt) = %s, %v, want %s", got, err, id)
	}
	if _, err := s.CreateNetwork(ctx, "mgmt", ""); !errors.Is(err, store.ErrConflict) {
		t.Errorf("duplicate create: err = %v, want ErrConflict", err)
	}
	if _, err := s.CreateNetwork(ctx, "default", ""); !errors.Is(err, store.ErrConflict) {
		t.Errorf("re-create 'default': err = %v, want ErrConflict", err)
	}
	if _, err := s.NetworkIDByName(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("NetworkIDByName(nope): err = %v, want ErrNotFound", err)
	}

	if err := s.UpdateNetwork(ctx, "mgmt", "Management plane"); err != nil {
		t.Fatalf("UpdateNetwork: %v", err)
	}
	if err := s.UpdateNetwork(ctx, "nope", "x"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UpdateNetwork(nope): err = %v, want ErrNotFound", err)
	}

	nets, err := s.ListNetworksConfig(ctx)
	if err != nil {
		t.Fatalf("ListNetworksConfig: %v", err)
	}
	// Sorted by name: default, mgmt.
	if len(nets) != 2 || nets[0].Name != "default" || nets[1].Name != "mgmt" ||
		nets[1].DisplayName != "Management plane" || nets[1].ID != id {
		t.Errorf("ListNetworksConfig = %+v, want default + updated mgmt", nets)
	}
}

func TestListNetworksConfigCounts(t *testing.T) {
	ctx, s := newStore(t)
	buildNetFixture(t, ctx, s)

	nets, err := s.ListNetworksConfig(ctx)
	if err != nil {
		t.Fatalf("ListNetworksConfig: %v", err)
	}
	byName := map[string]store.NetworkAdminInfo{}
	for _, n := range nets {
		byName[n.Name] = n
	}
	// Fixture: 2 default agents + 2 mgmt agents, all via consumed tokens;
	// mesh m1 and the direct probe live on default.
	def, mgmt := byName["default"], byName["mgmt"]
	if def.AgentCount != 2 || def.TokenCount != 2 || def.MeshCount != 1 || def.ProbeCount != 1 {
		t.Errorf("default counts = %+v, want 2 agents, 2 tokens, 1 mesh, 1 probe", def)
	}
	if mgmt.AgentCount != 2 || mgmt.TokenCount != 2 || mgmt.MeshCount != 0 || mgmt.ProbeCount != 0 {
		t.Errorf("mgmt counts = %+v, want 2 agents, 2 tokens, 0 meshes, 0 probes", mgmt)
	}
}

func TestDeleteNetwork(t *testing.T) {
	ctx, s := newStore(t)

	// Unreferenced network deletes cleanly.
	if _, err := s.CreateNetwork(ctx, "empty", ""); err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	if n, err := s.DeleteNetwork(ctx, "empty"); err != nil || n != 0 {
		t.Errorf("delete empty network = %d, %v, want 0, nil", n, err)
	}

	// Unused tokens are swept with the network.
	tok := createNetwork(t, ctx, s, "tok")
	siteID, err := s.EnsureSite(ctx, "site-a")
	if err != nil {
		t.Fatalf("EnsureSite: %v", err)
	}
	token, err := s.CreateJoinToken(ctx, siteID, "test", time.Hour)
	if err != nil {
		t.Fatalf("CreateJoinToken: %v", err)
	}
	tokenID, _, _ := strings.Cut(token, ".")
	if _, err := s.Pool().Exec(ctx,
		`UPDATE join_tokens SET network_id = $2 WHERE id = $1`, tokenID, tok); err != nil {
		t.Fatalf("move token: %v", err)
	}
	if n, err := s.DeleteNetwork(ctx, "tok"); err != nil || n != 1 {
		t.Errorf("delete network with unused token = %d, %v, want 1, nil", n, err)
	}
	var tokensLeft int
	if err := s.Pool().QueryRow(ctx,
		`SELECT count(*) FROM join_tokens WHERE id = $1`, tokenID).Scan(&tokensLeft); err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	if tokensLeft != 0 {
		t.Errorf("swept token still present after delete")
	}

	// The seeded fallback is never deletable.
	if _, err := s.DeleteNetwork(ctx, "default"); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("delete 'default': err = %v, want ErrInvalid", err)
	}
	if _, err := s.DeleteNetwork(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("delete unknown: err = %v, want ErrNotFound", err)
	}

	// An enrolled agent blocks the delete, and the refusal rolls back the
	// unused-token sweep.
	used := createNetwork(t, ctx, s, "used")
	enrollNetAgent(t, ctx, s, "site-a", "u1", &used)
	spare, err := s.CreateJoinToken(ctx, siteID, "test", time.Hour)
	if err != nil {
		t.Fatalf("CreateJoinToken: %v", err)
	}
	spareID, _, _ := strings.Cut(spare, ".")
	if _, err := s.Pool().Exec(ctx,
		`UPDATE join_tokens SET network_id = $2 WHERE id = $1`, spareID, used); err != nil {
		t.Fatalf("move token: %v", err)
	}
	if _, err := s.DeleteNetwork(ctx, "used"); !errors.Is(err, store.ErrConflict) {
		t.Errorf("delete network with agent: err = %v, want ErrConflict", err)
	}
	if err := s.Pool().QueryRow(ctx,
		`SELECT count(*) FROM join_tokens WHERE id = $1`, spareID).Scan(&tokensLeft); err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	if tokensLeft != 1 {
		t.Errorf("refused delete did not roll back the unused-token sweep")
	}

	// A mesh group blocks the delete.
	meshNet := createNetwork(t, ctx, s, "meshnet")
	if _, err := s.UpsertMeshGroup(ctx, "m-del"); err != nil {
		t.Fatalf("UpsertMeshGroup: %v", err)
	}
	moveMeshToNetwork(t, ctx, s, "m-del", meshNet)
	if _, err := s.DeleteNetwork(ctx, "meshnet"); !errors.Is(err, store.ErrConflict) {
		t.Errorf("delete network with mesh: err = %v, want ErrConflict", err)
	}

	// A direct probe config blocks the delete.
	probeNet := createNetwork(t, ctx, s, "probenet")
	if _, err := s.UpsertExternalTarget(ctx, "svc-del", "203.0.113.9", 443, ""); err != nil {
		t.Fatalf("UpsertExternalTarget: %v", err)
	}
	probeID, err := s.AddDirectProbe(ctx, "site-a", "svc-del", netProbeSettings, true, "test")
	if err != nil {
		t.Fatalf("AddDirectProbe: %v", err)
	}
	if _, err := s.Pool().Exec(ctx,
		`UPDATE probe_configs SET network_id = $2 WHERE id = $1`, probeID, probeNet); err != nil {
		t.Fatalf("move probe: %v", err)
	}
	if _, err := s.DeleteNetwork(ctx, "probenet"); !errors.Is(err, store.ErrConflict) {
		t.Errorf("delete network with probe config: err = %v, want ErrConflict", err)
	}
}
