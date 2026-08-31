package grpcapi

// DB-backed tests for StreamConfig's five behaviors: initial-send
// suppression, the fail-closed revocation sweep, the config_version rebuild
// gate, the forcedRebuildTicks backstop, and rebuild-failure-keeps-last-
// snapshot. Gated on POLARBEAM_TEST_DB_URL (see internal/server/dbtest).
//
// The store field is concrete, so everything runs over a real database; the
// gRPC layer is faked instead: identity comes from a hand-built peer context
// (the server never verifies chains itself — production TLS does), the
// stream is an in-memory collector, and the liveness tick is an UNBUFFERED
// channel supplied through the streamTicker seam. That unbuffered channel is
// the synchronization backbone: sending tick N+1 cannot complete until the
// loop finished processing tick N and re-entered select, so tick sends
// double as barriers and no test ever sleeps or polls.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
	"github.com/devalexllc/polarbeam/internal/server/ca"
	"github.com/devalexllc/polarbeam/internal/server/dbtest"
	"github.com/devalexllc/polarbeam/internal/server/store"
)

// fakeConfigStream collects sends; the embedded nil ServerStream covers the
// interface methods StreamConfig never calls.
type fakeConfigStream struct {
	grpc.ServerStream
	ctx  context.Context
	mu   sync.Mutex
	sent []*pb.ConfigSnapshot
}

func (f *fakeConfigStream) Context() context.Context { return f.ctx }

func (f *fakeConfigStream) Send(s *pb.ConfigSnapshot) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, s)
	return nil
}

func (f *fakeConfigStream) snapshots() []*pb.ConfigSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*pb.ConfigSnapshot(nil), f.sent...)
}

// streamHarness provisions a migrated database with a store handle and a raw
// pool for behind-the-store's-back SQL.
func streamHarness(t *testing.T) (context.Context, *store.Store, *pgxpool.Pool) {
	t.Helper()
	dburl := dbtest.Migrated(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	s, err := store.Connect(ctx, dburl, 10*time.Second, 0)
	if err != nil {
		t.Fatalf("store.Connect: %v", err)
	}
	t.Cleanup(s.Close)
	raw, err := pgxpool.New(ctx, dburl)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(raw.Close)
	return ctx, s, raw
}

// streamSetup enrolls a two-site mesh with one probe on the default network
// so the enrolled agent's snapshot is non-trivial, and returns that agent's
// identity (ID + certificate serial).
func streamSetup(t *testing.T, ctx context.Context, s *store.Store) (uuid.UUID, *big.Int) {
	t.Helper()
	network, err := s.NetworkIDByName(ctx, "default")
	if err != nil {
		t.Fatalf("NetworkIDByName: %v", err)
	}
	agentID := ingestEnroll(t, ctx, s, "site-a", "stream-a", network)
	ingestEnroll(t, ctx, s, "site-b", "stream-b", network)
	if _, err := s.UpsertMeshGroup(ctx, "stream-mesh", &network); err != nil {
		t.Fatalf("UpsertMeshGroup: %v", err)
	}
	for _, site := range []string{"site-a", "site-b"} {
		if err := s.AddMeshMember(ctx, "stream-mesh", site, nil); err != nil {
			t.Fatalf("AddMeshMember %s: %v", site, err)
		}
	}
	if _, err := s.AddMeshProbe(ctx, "stream-mesh", store.ProbeSettings{
		ProbeType: 1, Interval: time.Minute, Timeout: 5 * time.Second,
		Params: map[string]string{},
	}, true, "test", nil); err != nil {
		t.Fatalf("AddMeshProbe: %v", err)
	}
	var serialStr string
	if err := s.Pool().QueryRow(ctx,
		`SELECT serial::text FROM certificates WHERE agent_id = $1`, agentID).
		Scan(&serialStr); err != nil {
		t.Fatalf("read certificate serial: %v", err)
	}
	serial, ok := new(big.Int).SetString(serialStr, 10)
	if !ok {
		t.Fatalf("certificate serial %q is not an integer", serialStr)
	}
	return agentID, serial
}

// agentPeerCtx fabricates the mTLS peer info authenticateAgent reads. The
// leaf is unsigned on purpose: chain verification happens in the TLS
// handshake in production, never in the server, so identity here is just
// the URI SAN plus the serial the certificates table knows.
func agentPeerCtx(ctx context.Context, agentID uuid.UUID, serial *big.Int) context.Context {
	leaf := &x509.Certificate{
		SerialNumber: serial,
		URIs:         []*url.URL{ca.AgentURISAN(agentID)},
	}
	return peer.NewContext(ctx, &peer.Peer{
		AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{
			VerifiedChains: [][]*x509.Certificate{{leaf}},
		}},
	})
}

type streamRun struct {
	stream *fakeConfigStream
	tick   chan time.Time
	err    chan error
	cancel context.CancelFunc
}

// startStream runs StreamConfig in a goroutine against a fake stream and an
// unbuffered tick channel.
func startStream(t *testing.T, ctx context.Context, srv *Server, agentID uuid.UUID, serial *big.Int, helloHash string) *streamRun {
	t.Helper()
	tick := make(chan time.Time) // unbuffered: sends are barriers
	srv.streamTicker = func() (<-chan time.Time, func()) { return tick, func() {} }
	streamCtx, cancel := context.WithCancel(agentPeerCtx(ctx, agentID, serial))
	t.Cleanup(cancel)
	r := &streamRun{
		stream: &fakeConfigStream{ctx: streamCtx},
		tick:   tick,
		err:    make(chan error, 1),
		cancel: cancel,
	}
	go func() {
		r.err <- srv.StreamConfig(&pb.AgentHello{AgentVersion: "vtest", ConfigHash: helloHash}, r.stream)
	}()
	return r
}

// tick feeds one liveness tick. Because the channel is unbuffered, returning
// from here guarantees every EARLIER tick was fully processed.
func (r *streamRun) tickOnce(t *testing.T) {
	t.Helper()
	select {
	case r.tick <- time.Now():
	case err := <-r.err:
		t.Fatalf("stream ended before the tick was consumed: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("stream never consumed the tick")
	}
}

// join cancels the stream context and requires a clean nil return. Only
// call it when the loop is provably idle in select (nothing in flight —
// e.g. after waitFor observed the phase's final TouchAgent): a cancel that
// lands mid-tick aborts the tick's own store calls and surfaces as the
// fail-closed Unavailable instead.
func (r *streamRun) join(t *testing.T) {
	t.Helper()
	r.cancel()
	select {
	case err := <-r.err:
		if err != nil {
			t.Fatalf("StreamConfig = %v, want nil on disconnect", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("StreamConfig did not return after ctx cancel")
	}
}

// stop tears the stream down without asserting the return value — for tests
// whose last tick may still be in flight when the cancel lands (the clean
// disconnect path is pinned once, in the suppression test).
func (r *streamRun) stop(t *testing.T) {
	t.Helper()
	r.cancel()
	select {
	case <-r.err:
	case <-time.After(10 * time.Second):
		t.Fatal("StreamConfig did not return after ctx cancel")
	}
}

// waitFor polls a condition with a bound — used only for effects that are
// guaranteed to happen (never as a probabilistic grace period).
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitInitial waits until the stream's initial phase is fully done: the
// first snapshot was sent AND its TouchAgent write is visible, i.e. the
// loop is idle in select with no tick in flight. Tests must reach this
// point before mutating the database (a mutation racing the initial build
// would poison the baseline) and before any action a tick could act on.
func waitInitial(ctx context.Context, t *testing.T, r *streamRun, raw *pgxpool.Pool, agentID uuid.UUID) string {
	t.Helper()
	waitFor(t, "the initial snapshot send", func() bool { return len(r.stream.snapshots()) == 1 })
	hash := r.stream.snapshots()[0].GetConfigHash()
	waitFor(t, "TouchAgent after the initial send", func() bool {
		return touched(ctx, t, raw, agentID, hash)
	})
	return hash
}

// touched reports whether TouchAgent has recorded the given hash — the last
// store call of both the initial phase and every tick, so observing it
// proves the loop is (about to be) idle in select.
func touched(ctx context.Context, t *testing.T, raw *pgxpool.Pool, agentID uuid.UUID, wantHash string) bool {
	t.Helper()
	var hash *string
	var lastSeen *time.Time
	if err := raw.QueryRow(ctx,
		`SELECT current_config_hash, last_seen_at FROM agents WHERE id = $1`, agentID).
		Scan(&hash, &lastSeen); err != nil {
		t.Fatalf("read agent row: %v", err)
	}
	return hash != nil && *hash == wantHash && lastSeen != nil
}

// joinErr waits for the stream to end on its own and returns the error.
func (r *streamRun) joinErr(t *testing.T) error {
	t.Helper()
	select {
	case err := <-r.err:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("StreamConfig did not return")
		return nil
	}
}

// Behavior 1: the initial send is suppressed when the agent already runs the
// current snapshot — but liveness (TouchAgent) still updates.
func TestStreamConfigInitialSendSuppressedOnMatchingHash(t *testing.T) {
	t.Parallel()
	ctx, s, raw := streamHarness(t)
	agentID, serial := streamSetup(t, ctx, s)
	srv := New(s, nil)

	// First connect with no cached hash: exactly one initial send. No ticks
	// are ever fed, so once TouchAgent's write is visible the loop is idle
	// in select and the disconnect must return a clean nil.
	r := startStream(t, ctx, srv, agentID, serial, "")
	hash := waitInitial(ctx, t, r, raw, agentID)
	if hash == "" {
		t.Fatal("snapshot carries an empty config_hash")
	}
	r.join(t)

	// Clear the liveness marker so the suppressed reconnect's TouchAgent is
	// distinguishable from the first connect's.
	if _, err := raw.Exec(ctx,
		`UPDATE agents SET current_config_hash = '', last_seen_at = NULL WHERE id = $1`, agentID); err != nil {
		t.Fatalf("reset agent liveness: %v", err)
	}

	// Reconnect already running that snapshot: nothing is sent, yet the
	// agent row is touched with the server's hash.
	r = startStream(t, ctx, srv, agentID, serial, hash)
	waitFor(t, "TouchAgent on the suppressed connect", func() bool {
		return touched(ctx, t, raw, agentID, hash)
	})
	if got := r.stream.snapshots(); len(got) != 0 {
		t.Errorf("connect with matching hash sent %d snapshots, want 0", len(got))
	}
	r.join(t)
}

// Behavior 2a: a revoked certificate drops the live stream on the next sweep
// tick with PermissionDenied — the DB is the sole revocation authority.
func TestStreamConfigRevocationSweepDropsStream(t *testing.T) {
	t.Parallel()
	ctx, s, raw := streamHarness(t)
	agentID, serial := streamSetup(t, ctx, s)
	srv := New(s, nil)

	r := startStream(t, ctx, srv, agentID, serial, "")
	waitInitial(ctx, t, r, raw, agentID) // no tick in flight when we revoke
	if err := s.RevokeCertificate(ctx, serial); err != nil {
		t.Fatalf("RevokeCertificate: %v", err)
	}
	r.tickOnce(t)
	if err := r.joinErr(t); status.Code(err) != codes.PermissionDenied {
		t.Errorf("post-revocation sweep returned %v, want PermissionDenied", err)
	}
}

// Behavior 2b: fail closed — a sweep that cannot CONFIRM validity drops the
// stream with Unavailable instead of letting it ride.
func TestStreamConfigSweepUnavailableOnDBError(t *testing.T) {
	t.Parallel()
	ctx, s, raw := streamHarness(t)
	agentID, serial := streamSetup(t, ctx, s)
	srv := New(s, nil)

	r := startStream(t, ctx, srv, agentID, serial, "")
	waitInitial(ctx, t, r, raw, agentID) // no tick in flight when we close
	s.Close()                            // every subsequent store call errors
	r.tickOnce(t)
	if err := r.joinErr(t); status.Code(err) != codes.Unavailable {
		t.Errorf("sweep with a dead DB returned %v, want Unavailable (fail closed)", err)
	}
}

// Behavior 3: rebuilds are gated on config_version — idle ticks never send,
// a store-mediated write converges in one tick, and a version bump whose
// rebuild yields the same hash sends nothing.
func TestStreamConfigVersionGatedRebuild(t *testing.T) {
	t.Parallel()
	ctx, s, raw := streamHarness(t)
	agentID, serial := streamSetup(t, ctx, s)
	srv := New(s, nil)

	r := startStream(t, ctx, srv, agentID, serial, "")
	r.tickOnce(t)
	r.tickOnce(t) // barrier: idle tick 1 fully processed
	if got := r.stream.snapshots(); len(got) != 1 {
		t.Fatalf("idle ticks sent %d snapshots, want just the initial 1", len(got))
	}

	// A store write that changes this agent's expansion: one more probe.
	if _, err := s.AddMeshProbe(ctx, "stream-mesh", store.ProbeSettings{
		ProbeType: 1, Interval: 2 * time.Minute, Timeout: 5 * time.Second,
		Params: map[string]string{},
	}, true, "test", nil); err != nil {
		t.Fatalf("AddMeshProbe: %v", err)
	}
	r.tickOnce(t)
	r.tickOnce(t) // barrier: the rebuild tick completed
	sent := r.stream.snapshots()
	if len(sent) != 2 {
		t.Fatalf("config write converged in %d sends, want 2 (initial + rebuild)", len(sent))
	}
	if sent[1].GetConfigHash() == sent[0].GetConfigHash() {
		t.Error("rebuild sent an unchanged config_hash")
	}
	// lastVer advanced: the next tick must not resend.
	r.tickOnce(t)

	// A version bump with no config change rebuilds to the same hash — and
	// sends nothing.
	if _, err := raw.Exec(ctx, `UPDATE config_version SET version = version + 1`); err != nil {
		t.Fatalf("bump config_version: %v", err)
	}
	r.tickOnce(t)
	r.tickOnce(t) // barrier: the same-hash rebuild tick completed
	if got := r.stream.snapshots(); len(got) != 2 {
		t.Errorf("same-hash rebuild sent %d snapshots, want still 2", len(got))
	}

	// The gate is CLOSED once lastVer catches up: a config edit made with
	// raw SQL (hash would change, but nothing bumps the version) must not
	// be rebuilt or sent on ordinary ticks — only the distant backstop may
	// pick it up. A stream whose lastVer never advanced would rebuild every
	// tick and leak this edit immediately.
	if _, err := raw.Exec(ctx,
		`UPDATE probe_configs SET interval_ms = interval_ms * 3`); err != nil {
		t.Fatalf("edit probe_configs behind the store's back: %v", err)
	}
	r.tickOnce(t)
	r.tickOnce(t) // barrier: the gate-closed tick completed
	if got := r.stream.snapshots(); len(got) != 2 {
		t.Errorf("closed version gate sent %d snapshots, want still 2 (unbumped edit leaked)", len(got))
	}
	r.stop(t)
}

// Behavior 4: config edited behind the store's back (raw SQL, no version
// bump) is caught by the forced-rebuild backstop after N ticks.
func TestStreamConfigForcedRebuildBackstop(t *testing.T) {
	t.Parallel()
	ctx, s, raw := streamHarness(t)
	agentID, serial := streamSetup(t, ctx, s)
	srv := New(s, nil)
	srv.forcedRebuildEvery = 3

	r := startStream(t, ctx, srv, agentID, serial, "")
	waitInitial(ctx, t, r, raw, agentID) // the edit must not poison the baseline build
	if _, err := raw.Exec(ctx,
		`UPDATE probe_configs SET interval_ms = interval_ms * 2`); err != nil {
		t.Fatalf("edit probe_configs behind the store's back: %v", err)
	}
	r.tickOnce(t)
	r.tickOnce(t) // barrier: tick 1 done; tick 2 (sinceRebuild=2 < 3) cannot send
	if got := r.stream.snapshots(); len(got) != 1 {
		t.Fatalf("ticks below the backstop sent %d snapshots, want just the initial 1", len(got))
	}
	r.tickOnce(t) // tick 3 hits the backstop and rebuilds
	r.tickOnce(t) // barrier: tick 3 done; tick 4 (sinceRebuild reset) cannot send
	sent := r.stream.snapshots()
	if len(sent) != 2 {
		t.Fatalf("backstop sent %d snapshots, want 2", len(sent))
	}
	if sent[1].GetConfigHash() == sent[0].GetConfigHash() {
		t.Error("forced rebuild sent an unchanged config_hash")
	}
	r.stop(t)
}

// Behavior 5: a failing rebuild keeps the last snapshot and the stream —
// liveness and revocation checking outlive config staleness.
func TestStreamConfigRebuildFailureKeepsStreamAlive(t *testing.T) {
	t.Parallel()
	ctx, s, raw := streamHarness(t)
	agentID, serial := streamSetup(t, ctx, s)
	srv := New(s, nil)
	srv.forcedRebuildEvery = 1 // force a rebuild attempt on every tick

	r := startStream(t, ctx, srv, agentID, serial, "")
	waitInitial(ctx, t, r, raw, agentID) // the initial build must succeed first
	// Break ONLY what LoadAgentConfigInputs reads: certificates and
	// config_version stay intact, so the sweep and version gate still work.
	if _, err := raw.Exec(ctx,
		`ALTER TABLE mesh_members RENAME TO mesh_members_broken`); err != nil {
		t.Fatalf("break the config loader: %v", err)
	}
	r.tickOnce(t)
	r.tickOnce(t)
	r.tickOnce(t) // barrier: two failed rebuild ticks fully processed
	select {
	case err := <-r.err:
		t.Fatalf("stream died on a rebuild failure: %v", err)
	default:
	}
	if got := r.stream.snapshots(); len(got) != 1 {
		t.Errorf("failed rebuilds sent %d snapshots, want just the initial 1", len(got))
	}
	r.stop(t)
}
