// Package server wires the control plane together: preflight checks, the two
// TLS listeners (agent gRPC with mTLS, dashboard HTTPS), and lifecycle.
package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pires/go-proxyproto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	"github.com/devalexllc/polarbeam/internal/server/ca"
	"github.com/devalexllc/polarbeam/internal/server/config"
	"github.com/devalexllc/polarbeam/internal/server/grpcapi"
	"github.com/devalexllc/polarbeam/internal/server/httpapi"
	"github.com/devalexllc/polarbeam/internal/server/migrate"
	"github.com/devalexllc/polarbeam/internal/server/outage"
	"github.com/devalexllc/polarbeam/internal/server/store"
	"github.com/devalexllc/polarbeam/web"
)

// Run performs preflight and serves until ctx is cancelled. Every preflight
// failure is fatal and names the problem.
func Run(ctx context.Context, cfg config.Config) error {
	authority, err := ca.Load(cfg.CA.Dir, ca.Lifetimes{Agent: cfg.CA.AgentCertLifetime, Server: cfg.CA.ServerCertLifetime})
	if err != nil {
		return fmt.Errorf("preflight: %w", err)
	}
	if cfg.CA.AgentCertLifetime < 24*time.Hour {
		slog.Warn("TEST MODE: ca.agent_cert_lifetime is shorter than 24h — agent certificates will churn rapidly; never run production this way",
			"agent_cert_lifetime", cfg.CA.AgentCertLifetime)
	}

	st, err := store.Connect(ctx, cfg.DB.URL, cfg.DB.ConnectTimeout, cfg.DB.MaxConns)
	if err != nil {
		return fmt.Errorf("preflight: %w", err)
	}
	defer st.Close()

	// A reachable but unmigrated database must never present as healthy.
	pending, err := migrate.Pending(ctx, st.Pool())
	if err != nil {
		return fmt.Errorf("preflight: %w", err)
	}
	if len(pending) > 0 {
		return fmt.Errorf("preflight: database schema is behind: %d pending migration(s) %v — run `polarbeam-server migrate` first",
			len(pending), pending)
	}

	// Percentiles are computed by TimescaleDB Toolkit (percentile_agg);
	// migration 0005 creates the extension, but a hand-built database or a
	// dropped extension must fail here, not on the first dashboard query.
	toolkit, err := st.ToolkitInstalled(ctx)
	if err != nil {
		return fmt.Errorf("preflight: %w", err)
	}
	if !toolkit {
		return fmt.Errorf("preflight: timescaledb_toolkit extension is not installed — percentiles require it; use the timescale/timescaledb-ha image (bundles it) or install the toolkit package, then run `polarbeam-server migrate`")
	}

	dashboardCert, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
	if err != nil {
		return fmt.Errorf("preflight: dashboard certificate (tls.cert_file/tls.key_file): %w", err)
	}

	grpcCert, err := ensureGRPCCert(authority, cfg.CA.Dir, cfg.Listen.GRPCHostname, cfg.CA.ServerCertLifetime)
	if err != nil {
		return fmt.Errorf("preflight: %w", err)
	}
	certProvider := &grpcCertProvider{cert: grpcCert}
	// Reissue on a timer too: startup-only rotation would let a server whose
	// uptime exceeds the cert lifetime serve an expired certificate.
	go certProvider.rotate(ctx, authority, cfg.CA.Dir, cfg.Listen.GRPCHostname, cfg.CA.ServerCertLifetime)

	api := grpcapi.New(st, authority)

	// Silence detection: agents that stop producing results AND stop
	// touching last_seen_at get a single agent_offline event. The same
	// sweep closes orphaned probe_failing events whose probe is no longer
	// assigned (nothing else can ever close them).
	go outage.Sweep(ctx, st.Pool(), outage.SweepConfig{
		AssignedProbeIDs: st.EnabledProbeIDs,
	})

	grpcTLS := newGRPCTLSConfig(certProvider.get, authority.Pool())
	grpcLis, err := net.Listen("tcp", cfg.Listen.GRPC)
	if err != nil {
		return fmt.Errorf("listen grpc %s: %w", cfg.Listen.GRPC, err)
	}
	grpcLis = maybeProxyProto(grpcLis, cfg.Listen.ProxyProtocol)
	grpcServer := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(grpcTLS)),
		grpc.MaxRecvMsgSize(16<<20),
		// Config streams are idle for long stretches; server pings keep
		// them alive through idle-timeout middleboxes (incl. our proxy's
		// proxy_timeout) and detect dead agents.
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    1 * time.Minute,
			Timeout: 20 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             30 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	api.Register(grpcServer)

	httpLis, err := net.Listen("tcp", cfg.Listen.HTTP)
	if err != nil {
		return fmt.Errorf("listen http %s: %w", cfg.Listen.HTTP, err)
	}
	httpLis = maybeProxyProto(httpLis, cfg.Listen.ProxyProtocol)
	httpServer := newDashboardServer(httpapi.New(st, web.Dist()), dashboardCert)

	errCh := make(chan error, 2)
	go func() {
		slog.Info("gRPC listening", "addr", cfg.Listen.GRPC, "hostname", cfg.Listen.GRPCHostname, "proxy_protocol", cfg.Listen.ProxyProtocol)
		errCh <- grpcServer.Serve(grpcLis)
	}()
	go func() {
		slog.Info("dashboard listening", "addr", cfg.Listen.HTTP, "proxy_protocol", cfg.Listen.ProxyProtocol)
		err := httpServer.ServeTLS(httpLis, "", "")
		if !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		slog.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		httpServer.Shutdown(shutdownCtx)
		// Agent config streams are intentionally long-lived, so GracefulStop
		// alone would wait on them forever; bound it and force-stop.
		stopped := make(chan struct{})
		go func() {
			grpcServer.GracefulStop()
			close(stopped)
		}()
		select {
		case <-stopped:
		case <-time.After(10 * time.Second):
			slog.Info("graceful stop timed out; closing active streams")
			grpcServer.Stop()
		}
		return nil
	case err := <-errCh:
		return err
	}
}

// needsReissue decides whether the auto-issued gRPC server certificate must
// be replaced: unreadable leaf, not signed by the current CA root (the CA
// was re-initialized, possibly with a new algorithm), hostname mismatch,
// less than 1/3 of lifetime remaining, or a chain without the CA
// (fingerprint-pinned enrollment needs the CA served in the handshake).
func needsReissue(leaf *x509.Certificate, chainLen int, hostname string, now time.Time, lifetime time.Duration, root *x509.Certificate) bool {
	if lifetime <= 0 {
		lifetime = ca.ServerCertLifetime
	}
	return leaf == nil ||
		leaf.CheckSignatureFrom(root) != nil ||
		leaf.VerifyHostname(hostname) != nil ||
		leaf.NotAfter.Sub(now) <= lifetime/3 ||
		chainLen < 2
}

// issueGRPCCert issues a fresh gRPC server certificate and persists it.
func issueGRPCCert(authority *ca.CA, dir, hostname string) (tls.Certificate, error) {
	certPath := filepath.Join(dir, "grpc-server.crt")
	keyPath := filepath.Join(dir, "grpc-server.key")
	certPEM, keyPEM, err := authority.IssueServerCert(hostname)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, fmt.Errorf("write %s: %w", keyPath, err)
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return tls.Certificate{}, fmt.Errorf("write %s: %w", certPath, err)
	}
	slog.Info("issued gRPC server certificate", "hostname", hostname)
	return tls.LoadX509KeyPair(certPath, keyPath)
}

// ensureGRPCCert loads the auto-issued gRPC server certificate, reissuing it
// when missing, hostname-mismatched, or past 2/3 of its lifetime.
func ensureGRPCCert(authority *ca.CA, dir, hostname string, lifetime time.Duration) (tls.Certificate, error) {
	certPath := filepath.Join(dir, "grpc-server.crt")
	keyPath := filepath.Join(dir, "grpc-server.key")
	if cert, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		if !needsReissue(cert.Leaf, len(cert.Certificate), hostname, time.Now(), lifetime, authority.Certificate()) {
			return cert, nil
		}
		slog.Info("reissuing gRPC server certificate",
			"reason", "expiring, not signed by the current CA, hostname change, missing CA in chain, or unreadable leaf")
	}
	return issueGRPCCert(authority, dir, hostname)
}

// grpcCertProvider hands the current gRPC server certificate to handshakes
// and swaps it when the rotation loop reissues.
type grpcCertProvider struct {
	mu   sync.RWMutex
	cert tls.Certificate
}

func (p *grpcCertProvider) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return &p.cert, nil
}

// rotate rechecks the served certificate periodically and reissues in place.
// A failed reissue is logged loudly and the old certificate keeps serving —
// existing connections are unaffected either way; only new handshakes see
// the swap.
func (p *grpcCertProvider) rotate(ctx context.Context, authority *ca.CA, dir, hostname string, lifetime time.Duration) {
	if lifetime <= 0 {
		lifetime = ca.ServerCertLifetime
	}
	interval := min(time.Hour, lifetime/12)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		p.mu.RLock()
		cur := p.cert
		p.mu.RUnlock()
		if !needsReissue(cur.Leaf, len(cur.Certificate), hostname, time.Now(), lifetime, authority.Certificate()) {
			continue
		}
		cert, err := issueGRPCCert(authority, dir, hostname)
		if err != nil {
			slog.Error("gRPC server certificate reissue failed; continuing with the current certificate", "error", err)
			continue
		}
		p.mu.Lock()
		p.cert = cert
		p.mu.Unlock()
		slog.Info("rotated gRPC server certificate", "hostname", hostname, "not_after", cert.Leaf.NotAfter)
	}
}

// newGRPCTLSConfig builds the agent gRPC listener's TLS config. TLS 1.3 and
// hybrid ML-KEM groups are pinned, not defaulted: key-exchange strength is a
// harvest-now-decrypt-later defence, so a peer whose TLS stack cannot do a
// post-quantum hybrid must fail the handshake rather than silently degrade
// to classical ECDH. Non-Go agents need X25519MLKEM768 or one of the listed
// NIST-curve hybrids.
func newGRPCTLSConfig(getCert func(*tls.ClientHelloInfo) (*tls.Certificate, error), clientCAs *x509.CertPool) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		CurvePreferences: []tls.CurveID{
			tls.X25519MLKEM768,
			tls.SecP256r1MLKEM768,
			tls.SecP384r1MLKEM1024,
		},
		GetCertificate: getCert,
		// Enrollment arrives with no client certificate; AgentService RPCs
		// enforce a verified cert themselves. Certs that ARE presented get
		// verified against the built-in CA here.
		ClientAuth: tls.VerifyClientCertIfGiven,
		ClientCAs:  clientCAs,
	}
}

// newDashboardServer builds the dashboard HTTPS server. The read timeouts
// bound the whole request read (slowloris/slow-body defence — the SNI
// passthrough proxy cannot; httpapi caps body size, so 30 s is generous).
// IdleTimeout is explicit because with ReadTimeout set and IdleTimeout
// zero, ReadTimeout would govern keepalive idle too and churn the SPA's
// polling connections. WriteTimeout stays unset: history queries and asset
// downloads may legitimately outlast any request-read bound.
func newDashboardServer(handler http.Handler, cert tls.Certificate) *http.Server {
	return &http.Server{
		Handler:           handler,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}},
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
}

// maybeProxyProto wraps lis so the PROXY protocol header sent by the edge
// proxy becomes each connection's RemoteAddr. REQUIRE, not lenient: a missing
// header means a misconfigured proxy and must fail loudly, and an optional
// header would let any network peer spoof its address past the login rate
// limiter. The header precedes the TLS handshake, so both TLS listeners
// (dashboard HTTPS, agent mTLS gRPC) wrap transparently.
func maybeProxyProto(lis net.Listener, enabled bool) net.Listener {
	if !enabled {
		return lis
	}
	return &proxyproto.Listener{
		Listener: lis,
		ConnPolicy: func(proxyproto.ConnPolicyOptions) (proxyproto.Policy, error) {
			return proxyproto.REQUIRE, nil
		},
		// Bounds the header read so a peer that connects and stalls
		// cannot hold an accept slot open indefinitely.
		ReadHeaderTimeout: 10 * time.Second,
	}
}
