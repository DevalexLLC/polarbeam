package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/devalexllc/polarbeam/internal/server/ca"
)

// TestNewDashboardServerTimeouts pins the hardened read timeouts. httptest
// handler tests bypass http.Server entirely, so the field values here are
// the only offline-checkable evidence the deadlines exist.
func TestNewDashboardServerTimeouts(t *testing.T) {
	srv := newDashboardServer(http.NotFoundHandler(), tls.Certificate{})
	if srv.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want 10s", srv.ReadHeaderTimeout)
	}
	if srv.ReadTimeout != 30*time.Second {
		t.Errorf("ReadTimeout = %v, want 30s", srv.ReadTimeout)
	}
	if srv.IdleTimeout != 2*time.Minute {
		t.Errorf("IdleTimeout = %v, want 2m", srv.IdleTimeout)
	}
	// WriteTimeout deliberately unset: it would cap slow history queries
	// and asset downloads, which ReadTimeout does not.
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0", srv.WriteTimeout)
	}
	if srv.TLSConfig.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %x, want TLS1.2", srv.TLSConfig.MinVersion)
	}
}

func TestNeedsReissue(t *testing.T) {
	now := time.Now()
	lifetime := 90 * 24 * time.Hour
	fresh := &x509.Certificate{
		DNSNames:  []string{"grpc.example.com"},
		NotBefore: now.Add(-time.Hour),
		NotAfter:  now.Add(lifetime),
	}
	expiring := &x509.Certificate{
		DNSNames:  []string{"grpc.example.com"},
		NotBefore: now.Add(-lifetime),
		NotAfter:  now.Add(lifetime/3 - time.Minute),
	}

	tests := []struct {
		name     string
		leaf     *x509.Certificate
		chainLen int
		hostname string
		lifetime time.Duration
		want     bool
	}{
		{"fresh cert kept", fresh, 2, "grpc.example.com", lifetime, false},
		{"nil leaf reissued", nil, 2, "grpc.example.com", lifetime, true},
		{"hostname mismatch reissued", fresh, 2, "other.example.com", lifetime, true},
		{"under third remaining reissued", expiring, 2, "grpc.example.com", lifetime, true},
		{"missing CA in chain reissued", fresh, 1, "grpc.example.com", lifetime, true},
		{"zero lifetime uses default", fresh, 2, "grpc.example.com", 0, false},
	}
	for _, tt := range tests {
		if got := needsReissue(tt.leaf, tt.chainLen, tt.hostname, now, tt.lifetime); got != tt.want {
			t.Errorf("%s: needsReissue = %v, want %v", tt.name, got, tt.want)
		}
	}

	// The default fallback must match the exported constant: a cert past
	// 2/3 of ServerCertLifetime reissues even when lifetime is zero.
	old := &x509.Certificate{
		DNSNames: []string{"grpc.example.com"},
		NotAfter: now.Add(ca.ServerCertLifetime/3 - time.Minute),
	}
	if !needsReissue(old, 2, "grpc.example.com", now, 0) {
		t.Error("zero lifetime did not fall back to the default for the expiry check")
	}
}

// TestNewGRPCTLSConfigPins pins the agent listener's TLS floor: TLS 1.3 with
// hybrid post-quantum key exchange only. Field values are the offline
// evidence; TestGRPCTLSHandshakeEnforcement proves they bite on the wire.
func TestNewGRPCTLSConfigPins(t *testing.T) {
	cfg := newGRPCTLSConfig(nil, nil)
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %x, want TLS1.3", cfg.MinVersion)
	}
	wantCurves := []tls.CurveID{tls.X25519MLKEM768, tls.SecP256r1MLKEM768, tls.SecP384r1MLKEM1024}
	if !slices.Equal(cfg.CurvePreferences, wantCurves) {
		t.Errorf("CurvePreferences = %v, want %v", cfg.CurvePreferences, wantCurves)
	}
	if cfg.ClientAuth != tls.VerifyClientCertIfGiven {
		t.Errorf("ClientAuth = %v, want VerifyClientCertIfGiven", cfg.ClientAuth)
	}
}

// TestGRPCTLSHandshakeEnforcement performs real loopback handshakes against
// newGRPCTLSConfig: a default Go client must land on TLS 1.3 with a hybrid
// ML-KEM group, while clients capped at TLS 1.2 or offering only classical
// curves must fail the handshake outright rather than degrade.
func TestGRPCTLSHandshakeEnforcement(t *testing.T) {
	dir := t.TempDir()
	if err := ca.Init(dir, false); err != nil {
		t.Fatalf("ca.Init: %v", err)
	}
	authority, err := ca.Load(dir, ca.Lifetimes{})
	if err != nil {
		t.Fatalf("ca.Load: %v", err)
	}
	certPEM, keyPEM, err := authority.IssueServerCert("grpc.test")
	if err != nil {
		t.Fatalf("IssueServerCert: %v", err)
	}
	serverCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}

	cfg := newGRPCTLSConfig(func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		return &serverCert, nil
	}, authority.Pool())
	lis, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	t.Cleanup(func() { lis.Close() })
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func() {
				// Drive the handshake explicitly; rejected clients error
				// here and the connection just closes.
				_ = conn.(*tls.Conn).HandshakeContext(context.Background())
				conn.Close()
			}()
		}
	}()

	clientCfg := func() *tls.Config {
		return &tls.Config{RootCAs: authority.Pool(), ServerName: "grpc.test"}
	}

	t.Run("default client negotiates hybrid TLS 1.3", func(t *testing.T) {
		conn, err := tls.Dial("tcp", lis.Addr().String(), clientCfg())
		if err != nil {
			t.Fatalf("handshake failed: %v", err)
		}
		defer conn.Close()
		state := conn.ConnectionState()
		if state.Version != tls.VersionTLS13 {
			t.Errorf("negotiated version = %x, want TLS1.3", state.Version)
		}
		if state.CurveID != tls.X25519MLKEM768 {
			t.Errorf("negotiated key exchange = %v, want X25519MLKEM768", state.CurveID)
		}
	})

	t.Run("TLS 1.2 client is rejected", func(t *testing.T) {
		c := clientCfg()
		c.MaxVersion = tls.VersionTLS12
		if conn, err := tls.Dial("tcp", lis.Addr().String(), c); err == nil {
			conn.Close()
			t.Error("TLS 1.2 handshake succeeded, want rejection")
		}
	})

	t.Run("classical-only key exchange is rejected", func(t *testing.T) {
		c := clientCfg()
		c.CurvePreferences = []tls.CurveID{tls.X25519, tls.CurveP256}
		if conn, err := tls.Dial("tcp", lis.Addr().String(), c); err == nil {
			conn.Close()
			t.Error("classical-curve handshake succeeded, want rejection")
		}
	})
}
