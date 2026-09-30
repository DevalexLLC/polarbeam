package store_test

import (
	"context"
	"errors"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/devalexllc/polarbeam/internal/server/store"
)

// enrollFixture mints a one-hour join token on the default network and
// returns it with an issue callback that records each minted serial.
type enrollFixture struct {
	token   string
	serials []*big.Int
}

func newEnrollFixture(t *testing.T, ctx context.Context, s *store.Store, site string) *enrollFixture {
	t.Helper()
	siteID, err := s.EnsureSite(ctx, site)
	if err != nil {
		t.Fatalf("EnsureSite: %v", err)
	}
	token, err := s.CreateJoinToken(ctx, siteID, networkIDByName(t, ctx, s, "default"), "test", time.Hour)
	if err != nil {
		t.Fatalf("CreateJoinToken: %v", err)
	}
	return &enrollFixture{token: token}
}

func (f *enrollFixture) issue(uuid.UUID) (store.IssuedCert, error) {
	serial := big.NewInt(certSerial.Add(1))
	f.serials = append(f.serials, serial)
	return store.IssuedCert{
		Serial:    serial,
		NotBefore: time.Now().Add(-time.Minute),
		NotAfter:  time.Now().Add(time.Hour),
	}, nil
}

func (f *enrollFixture) enroll(ctx context.Context, s *store.Store, csrHash string) (uuid.UUID, error) {
	agentID, _, err := s.EnrollAgent(ctx, f.token, "replay-host", "127.0.0.1", "v0", []byte(csrHash), f.issue)
	return agentID, err
}

// TestEnrollReplayAfterRevocationBaseline pins the core contract behind the
// lost-response replay path: once an operator revokes the agent's
// certificate, the consumed token plus the original CSR must not mint a
// fresh, unrevoked certificate. Revocation is plain SQL here — the path
// operators have always had — so the test is independent of any store
// revocation helper.
func TestEnrollReplayAfterRevocationBaseline(t *testing.T) {
	ctx, s := newStore(t)
	f := newEnrollFixture(t, ctx, s, "replay-baseline")
	agentID, err := f.enroll(ctx, s, "same-csr")
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if _, err := s.Pool().Exec(ctx,
		`UPDATE certificates SET revoked_at = now() WHERE agent_id = $1`, agentID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	_, err = f.enroll(ctx, s, "same-csr")
	if errors.Is(err, store.ErrTokenInvalid) {
		return
	}
	if err != nil {
		t.Fatalf("replay: unexpected error %v", err)
	}
	serial := f.serials[len(f.serials)-1]
	valid, verr := s.CertValid(ctx, serial, agentID)
	t.Fatalf("replay after revocation succeeded, new certificate valid=%v (check err %v)", valid, verr)
}

// waitLockWait blocks until some backend in the test database is waiting
// on a lock — held by holderPID when it is non-zero — proving the competing
// statement really reached the lock before the test releases the holder.
// A non-contending (broken) implementation never waits, and the bounded
// poll fails the test.
func waitLockWait(t *testing.T, ctx context.Context, s *store.Store, holderPID int32) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		err := s.Pool().QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database()
			  AND cardinality(pg_blocking_pids(pid)) > 0
			  AND ($1 = 0 OR $1 = ANY(pg_blocking_pids(pid)))`, holderPID).Scan(&n)
		if err != nil {
			t.Fatalf("lock-wait poll: %v", err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no backend ever waited on the lock: issuance and revocation do not serialize")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// gate parks an issue callback inside its transaction until opened.
type gate struct {
	c    chan struct{}
	once sync.Once
}

func (g *gate) open() { g.once.Do(func() { close(g.c) }) }

// releaser returns a gate that is also opened at cleanup, which runs before
// newStore's pool Close (cleanups are LIFO): a failing test must not leave
// a parked transaction holding a pooled connection that Close waits on.
func releaser(t *testing.T) *gate {
	g := &gate{c: make(chan struct{})}
	t.Cleanup(g.open)
	return g
}

// revokeInRawTx opens a transaction that revokes agentID the way the store
// does (agent row FOR UPDATE, then the UPDATE) and leaves it open. It
// returns the transaction and its backend pid.
func revokeInRawTx(t *testing.T, ctx context.Context, s *store.Store, agentID uuid.UUID) (pgx.Tx, int32) {
	t.Helper()
	tx, err := s.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	var pid int32
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("backend pid: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM agents WHERE id = $1 FOR UPDATE`, agentID); err != nil {
		t.Fatalf("lock agent: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE certificates SET revoked_at = now() WHERE agent_id = $1 AND revoked_at IS NULL`, agentID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	return tx, pid
}

// awaitIssue waits until an operation parked in its issue callback, failing
// fast if it returned without getting there.
func awaitIssue(t *testing.T, inIssue <-chan struct{}, opErr <-chan error) {
	t.Helper()
	select {
	case <-inIssue:
	case err := <-opErr:
		t.Fatalf("operation returned before issuing: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("operation never reached its issue callback")
	}
}

func mustValid(t *testing.T, ctx context.Context, s *store.Store, serial *big.Int, agentID uuid.UUID, want bool) {
	t.Helper()
	valid, err := s.CertValid(ctx, serial, agentID)
	if err != nil {
		t.Fatalf("CertValid: %v", err)
	}
	if valid != want {
		t.Fatalf("serial %s valid=%v, want %v", serial, valid, want)
	}
}

func certCount(t *testing.T, ctx context.Context, s *store.Store, agentID uuid.UUID) int {
	t.Helper()
	var n int
	if err := s.Pool().QueryRow(ctx,
		`SELECT count(*) FROM certificates WHERE agent_id = $1`, agentID).Scan(&n); err != nil {
		t.Fatalf("count certificates: %v", err)
	}
	return n
}

func TestEnrollFresh(t *testing.T) {
	ctx, s := newStore(t)
	f := newEnrollFixture(t, ctx, s, "enroll-fresh")
	agentID, err := f.enroll(ctx, s, "csr")
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	mustValid(t, ctx, s, f.serials[0], agentID, true)
	var used bool
	if err := s.Pool().QueryRow(ctx,
		`SELECT used_at IS NOT NULL FROM join_tokens WHERE used_by_agent = $1`, agentID).Scan(&used); err != nil || !used {
		t.Fatalf("token not consumed (used=%v, err=%v)", used, err)
	}
}

// A lost enroll response: the same token and CSR get the same agent back
// with a fresh valid certificate, and nothing is created twice.
func TestEnrollReplaySameCSRBeforeRevocation(t *testing.T) {
	ctx, s := newStore(t)
	f := newEnrollFixture(t, ctx, s, "enroll-retry")
	first, err := f.enroll(ctx, s, "csr")
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	again, err := f.enroll(ctx, s, "csr")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if again != first {
		t.Fatalf("replay returned agent %s, want %s", again, first)
	}
	mustValid(t, ctx, s, f.serials[1], first, true)
	var agents, targets int
	if err := s.Pool().QueryRow(ctx, `
		SELECT (SELECT count(*) FROM agents WHERE id = $1),
		       (SELECT count(*) FROM targets WHERE agent_id = $1)`, first).Scan(&agents, &targets); err != nil {
		t.Fatalf("count: %v", err)
	}
	if agents != 1 || targets != 1 {
		t.Fatalf("agents=%d targets=%d after replay, want 1 and 1", agents, targets)
	}
}

func TestEnrollReplayWrongCSR(t *testing.T) {
	ctx, s := newStore(t)
	f := newEnrollFixture(t, ctx, s, "enroll-wrong-csr")
	if _, err := f.enroll(ctx, s, "csr"); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	_, err := f.enroll(ctx, s, "other-csr")
	if !errors.Is(err, store.ErrTokenInvalid) || errors.Is(err, store.ErrEnrollRevoked) {
		t.Fatalf("wrong-CSR replay: got %v, want plain ErrTokenInvalid", err)
	}
}

func TestEnrollReplayAfterRevocation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		revoke func(t *testing.T, ctx context.Context, s *store.Store, agentID uuid.UUID, serial *big.Int)
	}{
		{"serial", func(t *testing.T, ctx context.Context, s *store.Store, _ uuid.UUID, serial *big.Int) {
			if _, _, err := s.RevokeCertificate(ctx, serial); err != nil {
				t.Fatalf("RevokeCertificate: %v", err)
			}
		}},
		{"agent", func(t *testing.T, ctx context.Context, s *store.Store, agentID uuid.UUID, _ *big.Int) {
			if _, err := s.RevokeAgentCertificates(ctx, agentID); err != nil {
				t.Fatalf("RevokeAgentCertificates: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, s := newStore(t)
			f := newEnrollFixture(t, ctx, s, "enroll-revoked-"+tc.name)
			agentID, err := f.enroll(ctx, s, "csr")
			if err != nil {
				t.Fatalf("enroll: %v", err)
			}
			tc.revoke(t, ctx, s, agentID, f.serials[0])
			_, err = f.enroll(ctx, s, "csr")
			if !errors.Is(err, store.ErrTokenInvalid) || !errors.Is(err, store.ErrEnrollRevoked) {
				t.Fatalf("replay after revocation: got %v, want ErrEnrollRevoked", err)
			}
			if len(f.serials) != 1 || certCount(t, ctx, s, agentID) != 1 {
				t.Fatalf("replay after revocation issued a certificate (serials %v)", f.serials)
			}
		})
	}
}

func TestEnrollReplayRacesRevocation(t *testing.T) {
	t.Run("replay first", func(t *testing.T) {
		ctx, s := newStore(t)
		f := newEnrollFixture(t, ctx, s, "race-replay-first")
		agentID, err := f.enroll(ctx, s, "csr")
		if err != nil {
			t.Fatalf("enroll: %v", err)
		}
		inIssue, release := make(chan struct{}), releaser(t)
		replayErr := make(chan error, 1)
		go func() {
			_, _, err := s.EnrollAgent(ctx, f.token, "replay-host", "127.0.0.1", "v0", []byte("csr"),
				func(id uuid.UUID) (store.IssuedCert, error) {
					close(inIssue)
					<-release.c
					return f.issue(id)
				})
			replayErr <- err
		}()
		awaitIssue(t, inIssue, replayErr) // the replay holds the agent lock and passed its check
		revoked := make(chan []string, 1)
		revokeErr := make(chan error, 1)
		go func() {
			serials, err := s.RevokeAgentCertificates(ctx, agentID)
			revoked <- serials
			revokeErr <- err
		}()
		waitLockWait(t, ctx, s, 0)
		release.open()
		if err := <-replayErr; err != nil {
			t.Fatalf("replay: %v", err)
		}
		if err := <-revokeErr; err != nil {
			t.Fatalf("revoke: %v", err)
		}
		if got := <-revoked; len(got) != 2 {
			t.Fatalf("revocation covered serials %v, want both the enrolled and the replayed one", got)
		}
		mustValid(t, ctx, s, f.serials[1], agentID, false)
	})

	t.Run("revoke first", func(t *testing.T) {
		ctx, s := newStore(t)
		f := newEnrollFixture(t, ctx, s, "race-revoke-first")
		agentID, err := f.enroll(ctx, s, "csr")
		if err != nil {
			t.Fatalf("enroll: %v", err)
		}
		tx, pid := revokeInRawTx(t, ctx, s, agentID)
		replayErr := make(chan error, 1)
		go func() {
			_, err := f.enroll(ctx, s, "csr")
			replayErr <- err
		}()
		waitLockWait(t, ctx, s, pid)
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit revocation: %v", err)
		}
		if err := <-replayErr; !errors.Is(err, store.ErrEnrollRevoked) {
			t.Fatalf("replay racing a committed revocation: got %v, want ErrEnrollRevoked", err)
		}
		if certCount(t, ctx, s, agentID) != 1 {
			t.Fatal("replay racing a revocation issued a certificate")
		}
	})
}

func TestRenewCertificate(t *testing.T) {
	setup := func(t *testing.T, site string) (context.Context, *store.Store, *enrollFixture, uuid.UUID) {
		ctx, s := newStore(t)
		f := newEnrollFixture(t, ctx, s, site)
		agentID, err := f.enroll(ctx, s, "csr")
		if err != nil {
			t.Fatalf("enroll: %v", err)
		}
		return ctx, s, f, agentID
	}
	renew := func(ctx context.Context, s *store.Store, f *enrollFixture, agentID uuid.UUID, old *big.Int) error {
		return s.RenewCertificate(ctx, agentID, old, func() (store.IssuedCert, error) { return f.issue(agentID) })
	}

	t.Run("valid", func(t *testing.T) {
		ctx, s, f, agentID := setup(t, "renew-valid")
		if err := renew(ctx, s, f, agentID, f.serials[0]); err != nil {
			t.Fatalf("renew: %v", err)
		}
		mustValid(t, ctx, s, f.serials[1], agentID, true)
	})

	t.Run("revoked", func(t *testing.T) {
		ctx, s, f, agentID := setup(t, "renew-revoked")
		if _, _, err := s.RevokeCertificate(ctx, f.serials[0]); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		if err := renew(ctx, s, f, agentID, f.serials[0]); !errors.Is(err, store.ErrCertRevoked) {
			t.Fatalf("renew with revoked serial: got %v, want ErrCertRevoked", err)
		}
		if len(f.serials) != 1 {
			t.Fatal("renewal with a revoked serial reached issuance")
		}
	})

	t.Run("unknown agent", func(t *testing.T) {
		ctx, s, f, _ := setup(t, "renew-unknown")
		if err := renew(ctx, s, f, uuid.New(), f.serials[0]); !errors.Is(err, store.ErrCertRevoked) {
			t.Fatalf("renew for unknown agent: got %v, want ErrCertRevoked", err)
		}
	})

	t.Run("renew first", func(t *testing.T) {
		ctx, s, f, agentID := setup(t, "renew-race-renew-first")
		inIssue, release := make(chan struct{}), releaser(t)
		renewErr := make(chan error, 1)
		go func() {
			renewErr <- s.RenewCertificate(ctx, agentID, f.serials[0], func() (store.IssuedCert, error) {
				close(inIssue)
				<-release.c
				return f.issue(agentID)
			})
		}()
		awaitIssue(t, inIssue, renewErr)
		revokeErr := make(chan error, 1)
		go func() {
			_, err := s.RevokeAgentCertificates(ctx, agentID)
			revokeErr <- err
		}()
		waitLockWait(t, ctx, s, 0)
		release.open()
		if err := <-renewErr; err != nil {
			t.Fatalf("renew: %v", err)
		}
		if err := <-revokeErr; err != nil {
			t.Fatalf("revoke: %v", err)
		}
		mustValid(t, ctx, s, f.serials[1], agentID, false)
	})

	// Validity is judged when the check runs, not when the transaction
	// began: a certificate that expires while renewal waits on the agent
	// row (here a plain lock, as ingest's dropped-total update takes) must
	// be refused.
	t.Run("expires during lock wait", func(t *testing.T) {
		ctx, s, f, agentID := setup(t, "renew-expires-waiting")
		if _, err := s.Pool().Exec(ctx, `
			UPDATE certificates SET not_after = now() + interval '3 seconds' WHERE serial = $1`,
			f.serials[0].String()); err != nil {
			t.Fatalf("shorten certificate: %v", err)
		}
		tx, err := s.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
		var pid int32
		if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			t.Fatalf("backend pid: %v", err)
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM agents WHERE id = $1 FOR UPDATE`, agentID); err != nil {
			t.Fatalf("lock agent: %v", err)
		}
		renewErr := make(chan error, 1)
		go func() { renewErr <- renew(ctx, s, f, agentID, f.serials[0]) }()
		waitLockWait(t, ctx, s, pid)
		deadline := time.Now().Add(10 * time.Second)
		for {
			var expired bool
			if err := s.Pool().QueryRow(ctx,
				`SELECT now() > not_after FROM certificates WHERE serial = $1`,
				f.serials[0].String()).Scan(&expired); err != nil {
				t.Fatalf("expiry poll: %v", err)
			}
			if expired {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("certificate never expired")
			}
			time.Sleep(50 * time.Millisecond)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("release lock: %v", err)
		}
		if err := <-renewErr; !errors.Is(err, store.ErrCertRevoked) {
			t.Fatalf("renewal of a certificate that expired while waiting: got %v, want ErrCertRevoked", err)
		}
	})

	t.Run("revoke first", func(t *testing.T) {
		ctx, s, f, agentID := setup(t, "renew-race-revoke-first")
		tx, pid := revokeInRawTx(t, ctx, s, agentID)
		renewErr := make(chan error, 1)
		go func() { renewErr <- renew(ctx, s, f, agentID, f.serials[0]) }()
		waitLockWait(t, ctx, s, pid)
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit revocation: %v", err)
		}
		if err := <-renewErr; !errors.Is(err, store.ErrCertRevoked) {
			t.Fatalf("renew racing a committed revocation: got %v, want ErrCertRevoked", err)
		}
		if certCount(t, ctx, s, agentID) != 1 {
			t.Fatal("renewal racing a revocation issued a certificate")
		}
	})
}

func TestRevokeCertificate(t *testing.T) {
	ctx, s := newStore(t)
	f := newEnrollFixture(t, ctx, s, "revoke-serial")
	agentID, err := f.enroll(ctx, s, "csr")
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if _, err := f.enroll(ctx, s, "csr"); err != nil { // lost-response retry: a second live serial
		t.Fatalf("replay: %v", err)
	}
	if _, _, err := s.RevokeCertificate(ctx, big.NewInt(-1)); err == nil {
		t.Fatal("revoking an unknown serial succeeded")
	}
	owner, live, err := s.RevokeCertificate(ctx, f.serials[0])
	if err != nil {
		t.Fatalf("RevokeCertificate: %v", err)
	}
	if owner != agentID || live != 1 {
		t.Fatalf("RevokeCertificate = (%s, %d), want (%s, 1)", owner, live, agentID)
	}
	mustValid(t, ctx, s, f.serials[1], agentID, true)
	if _, _, err := s.RevokeCertificate(ctx, f.serials[0]); err == nil {
		t.Fatal("revoking an already-revoked serial succeeded")
	}
}

func TestRevokeAgentCertificates(t *testing.T) {
	ctx, s := newStore(t)
	if _, err := s.RevokeAgentCertificates(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown agent: got %v, want ErrNotFound", err)
	}
	f := newEnrollFixture(t, ctx, s, "revoke-agent")
	agentID, err := f.enroll(ctx, s, "csr")
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if _, err := f.enroll(ctx, s, "csr"); err != nil {
		t.Fatalf("replay: %v", err)
	}
	serials, err := s.RevokeAgentCertificates(ctx, agentID)
	if err != nil || len(serials) != 2 {
		t.Fatalf("RevokeAgentCertificates = (%v, %v), want two serials", serials, err)
	}
	for _, serial := range f.serials {
		mustValid(t, ctx, s, serial, agentID, false)
	}
	if serials, err := s.RevokeAgentCertificates(ctx, agentID); err != nil || len(serials) != 0 {
		t.Fatalf("second RevokeAgentCertificates = (%v, %v), want (none, nil)", serials, err)
	}
}
