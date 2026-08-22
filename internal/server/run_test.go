package server

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/devalexllc/polarbeam/internal/server/ca"
	"github.com/google/uuid"
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

// testRoot self-signs a throwaway CA certificate; needsReissue verifies the
// leaf's signature against the current root, so the test certs must be
// really signed rather than bare structs.
func testRoot(t *testing.T, now time.Time) (*x509.Certificate, crypto.Signer) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test root"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour * 3650),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

func testLeaf(t *testing.T, root *x509.Certificate, rootKey crypto.Signer, hostname string, notBefore, notAfter time.Time) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: hostname},
		DNSNames:     []string{hostname},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, root, key.Public(), rootKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func TestNeedsReissue(t *testing.T) {
	now := time.Now()
	lifetime := 90 * 24 * time.Hour
	root, rootKey := testRoot(t, now)
	otherRoot, _ := testRoot(t, now)
	fresh := testLeaf(t, root, rootKey, "grpc.example.com", now.Add(-time.Hour), now.Add(lifetime))
	expiring := testLeaf(t, root, rootKey, "grpc.example.com", now.Add(-lifetime), now.Add(lifetime/3-time.Minute))

	tests := []struct {
		name     string
		leaf     *x509.Certificate
		chainLen int
		hostname string
		lifetime time.Duration
		root     *x509.Certificate
		want     bool
	}{
		{"fresh cert kept", fresh, 2, "grpc.example.com", lifetime, root, false},
		{"nil leaf reissued", nil, 2, "grpc.example.com", lifetime, root, true},
		{"hostname mismatch reissued", fresh, 2, "other.example.com", lifetime, root, true},
		{"under third remaining reissued", expiring, 2, "grpc.example.com", lifetime, root, true},
		{"missing CA in chain reissued", fresh, 1, "grpc.example.com", lifetime, root, true},
		{"zero lifetime uses default", fresh, 2, "grpc.example.com", 0, root, false},
		// The CA was re-initialized (e.g. an algorithm cutover): a leaf
		// chained to the retired root must be replaced immediately, not
		// served until it expires.
		{"signed by a different root reissued", fresh, 2, "grpc.example.com", lifetime, otherRoot, true},
	}
	for _, tt := range tests {
		if got := needsReissue(tt.leaf, tt.chainLen, tt.hostname, now, tt.lifetime, tt.root); got != tt.want {
			t.Errorf("%s: needsReissue = %v, want %v", tt.name, got, tt.want)
		}
	}

	// The default fallback must match the exported constant: a cert past
	// 2/3 of ServerCertLifetime reissues even when lifetime is zero.
	old := testLeaf(t, root, rootKey, "grpc.example.com",
		now.Add(-ca.ServerCertLifetime), now.Add(ca.ServerCertLifetime/3-time.Minute))
	if !needsReissue(old, 2, "grpc.example.com", now, 0, root) {
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
// ML-KEM group and an ML-DSA-65 server certificate (the default CA
// algorithm), while clients capped at TLS 1.2 or offering only classical
// curves must fail the handshake outright rather than degrade.
func TestGRPCTLSHandshakeEnforcement(t *testing.T) {
	dir := t.TempDir()
	if err := ca.Init(dir, ca.DefaultAlgorithm, false); err != nil {
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
		if got := state.PeerCertificates[0].PublicKeyAlgorithm; got != x509.MLDSA {
			t.Errorf("server certificate public key algorithm = %v, want ML-DSA", got)
		}
	})

	t.Run("ML-DSA client certificate verifies for mTLS", func(t *testing.T) {
		agentKey, err := mldsa.GenerateKey(mldsa.MLDSA65())
		if err != nil {
			t.Fatal(err)
		}
		csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, agentKey)
		if err != nil {
			t.Fatal(err)
		}
		agentID := uuid.New()
		der, _, _, err := authority.SignAgentCSR(csrDER, agentID, "agent.test")
		if err != nil {
			t.Fatalf("SignAgentCSR: %v", err)
		}

		// Dedicated listener so the server side of this one handshake can
		// report the verified client certificate.
		mtlsLis, err := tls.Listen("tcp", "127.0.0.1:0", cfg.Clone())
		if err != nil {
			t.Fatalf("tls.Listen: %v", err)
		}
		t.Cleanup(func() { mtlsLis.Close() })
		peerCerts := make(chan []*x509.Certificate, 1)
		go func() {
			conn, err := mtlsLis.Accept()
			if err != nil {
				close(peerCerts)
				return
			}
			defer conn.Close()
			tc := conn.(*tls.Conn)
			if err := tc.HandshakeContext(context.Background()); err != nil {
				close(peerCerts)
				return
			}
			peerCerts <- tc.ConnectionState().PeerCertificates
		}()

		c := clientCfg()
		c.Certificates = []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: agentKey}}
		conn, err := tls.Dial("tcp", mtlsLis.Addr().String(), c)
		if err != nil {
			t.Fatalf("mTLS handshake failed: %v", err)
		}
		defer conn.Close()
		if err := conn.HandshakeContext(context.Background()); err != nil {
			t.Fatalf("client handshake: %v", err)
		}

		peers, ok := <-peerCerts
		if !ok || len(peers) == 0 {
			t.Fatal("server did not receive a client certificate")
		}
		if got := peers[0].PublicKeyAlgorithm; got != x509.MLDSA {
			t.Errorf("client certificate public key algorithm = %v, want ML-DSA", got)
		}
		gotID, err := ca.AgentIDFromCert(peers[0])
		if err != nil {
			t.Fatalf("AgentIDFromCert: %v", err)
		}
		if gotID != agentID {
			t.Errorf("agent identity = %s, want %s", gotID, agentID)
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
