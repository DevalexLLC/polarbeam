package uplink

import (
	"context"
	"crypto"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/devalexllc/polarbeam/internal/agent/enroll"
	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
)

const renewTimeout = 30 * time.Second

// Renewer keeps the agent certificate fresh: it sleeps until 2/3 of the
// leaf's actual validity has passed, calls RenewCert over the existing mTLS
// channel (reusing the current private key — see PKI.RenewalCSR), commits
// the new certificate atomically, and recycles the connection so the
// renewal takes effect immediately instead of at old-cert expiry. The
// schedule is re-derived from whatever leaf is on disk, so restarts and
// crashes resume correctly with no extra state.
type Renewer struct {
	pki enroll.PKI
	// renew is the transport seam (RenewCert over the uplink in
	// production, stubbed in tests).
	renew func(ctx context.Context, csrDER []byte) (*pb.RenewCertResponse, error)
	// onRenewed runs after a successful commit (Uplink.Recycle in
	// production).
	onRenewed func() error
	now       func() time.Time
}

// NewRenewer wires a Renewer to this uplink's connection and the agent's
// PKI state.
func (u *Uplink) NewRenewer() *Renewer {
	return &Renewer{
		pki: enroll.NewPKI(u.cfg.StateDir),
		renew: func(ctx context.Context, csrDER []byte) (*pb.RenewCertResponse, error) {
			ctx, cancel := context.WithTimeout(ctx, renewTimeout)
			defer cancel()
			return pb.NewAgentServiceClient(u.getConn()).RenewCert(ctx,
				&pb.RenewCertRequest{CsrDer: csrDER})
		},
		onRenewed: u.Recycle,
		now:       time.Now,
	}
}

// renewAtFor is the renewal point: 2/3 of the leaf's actual validity. The
// agent deliberately derives this from the certificate rather than config,
// so a server-side lifetime change (including the 10m test mode) needs no
// agent knob and cannot disagree with what was actually issued.
func renewAtFor(leaf *x509.Certificate) time.Time {
	return leaf.NotBefore.Add(leaf.NotAfter.Sub(leaf.NotBefore) * 2 / 3)
}

// retryIntervalFor scales the failure-retry cadence with cert validity:
// daily for the production 30d certs, ~45s for a 10m test cert, never more
// often than every 30s.
func retryIntervalFor(validity time.Duration) time.Duration {
	return min(24*time.Hour, max(30*time.Second, validity/20))
}

// Run renews until ctx is cancelled. Failures are logged loudly and retried
// on the validity-scaled interval; an agent dark past expiry cannot renew
// (the server rejects expired client certs) and must be re-enrolled — the
// loop keeps saying so rather than giving up silently.
func (r *Renewer) Run(ctx context.Context) {
	for ctx.Err() == nil {
		leaf, err := r.pki.Leaf()
		if err != nil {
			slog.Error("certificate renewal: cannot read agent certificate", "err", err,
				"retry_in", time.Minute)
			if !r.sleep(ctx, time.Minute) {
				return
			}
			continue
		}
		validity := leaf.NotAfter.Sub(leaf.NotBefore)
		now := r.now()

		if wait := renewAtFor(leaf).Sub(now); wait > 0 {
			// ±5% jitter so a fleet enrolled together doesn't renew in
			// lockstep.
			if !r.sleep(ctx, jitterPct(wait, 0.05)) {
				return
			}
			continue // re-read the leaf: it may have been replaced meanwhile
		}

		if now.After(leaf.NotAfter) {
			slog.Error("agent certificate is expired — the server rejects expired certificates; " +
				"re-enroll with a fresh token (polarbeam-agent enroll)")
		}
		if err := r.renewOnce(ctx); err != nil {
			retry := retryIntervalFor(validity)
			slog.Error("certificate renewal failed", "err", err, "retry_in", retry)
			if !r.sleep(ctx, jitterPct(retry, 0.05)) {
				return
			}
		}
	}
}

// renewOnce performs a single CSR → RenewCert → commit → recycle cycle.
func (r *Renewer) renewOnce(ctx context.Context) error {
	csrDER, err := r.pki.RenewalCSR()
	if err != nil {
		return err
	}
	resp, err := r.renew(ctx, csrDER)
	if err != nil {
		return err
	}
	// Never commit blind: an unparseable or wrong-key certificate would
	// break the on-disk matching key/cert invariant and brick the uplink.
	renewed, err := x509.ParseCertificate(resp.GetCertDer())
	if err != nil {
		return fmt.Errorf("server returned an unparseable certificate: %w", err)
	}
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return err
	}
	pub, ok := renewed.PublicKey.(interface{ Equal(crypto.PublicKey) bool })
	if !ok || !pub.Equal(csr.PublicKey) {
		return errors.New("server returned a certificate for a different key")
	}
	if err := r.pki.CommitRenewal(resp.GetCertDer(), resp.GetCaBundleDer()); err != nil {
		return err
	}
	slog.Info("certificate renewed", "not_after", resp.GetNotAfter().AsTime().Format(time.RFC3339))
	if r.onRenewed != nil {
		if err := r.onRenewed(); err != nil {
			// The renewed cert is committed; without the recycle it still
			// takes effect on the next natural reconnect handshake.
			slog.Error("connection recycle after renewal failed; renewed certificate applies on next reconnect", "err", err)
		}
	}
	return nil
}

func (r *Renewer) sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func jitterPct(d time.Duration, pct float64) time.Duration {
	f := 1 - pct + 2*pct*rand.Float64()
	return time.Duration(float64(d) * f)
}
