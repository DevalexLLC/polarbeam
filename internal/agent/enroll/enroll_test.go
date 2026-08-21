package enroll

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/devalexllc/polarbeam/internal/agent/config"
)

func genKeyCSR(t *testing.T) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}
	return key, csr
}

func genServerCertificate(t *testing.T, hostname string) (tls.Certificate, []byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: hostname},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		DNSNames:              []string{hostname},
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return cert, certPEM, keyPEM
}

func TestTLSConfigsPinHybridCurvePreferences(t *testing.T) {
	_, certPEM, keyPEM := genServerCertificate(t, "grpc.test")
	cfg := config.Config{Server: config.Server{Address: "grpc.test:443"}}
	want := []tls.CurveID{
		tls.X25519MLKEM768,
		tls.SecP256r1MLKEM768,
		tls.SecP384r1MLKEM1024,
	}
	assertPolicy := func(t *testing.T, tlsCfg *tls.Config) {
		t.Helper()
		if tlsCfg.MinVersion != tls.VersionTLS13 {
			t.Errorf("MinVersion = %x, want TLS1.3", tlsCfg.MinVersion)
		}
		if !slices.Equal(tlsCfg.CurvePreferences, want) {
			t.Errorf("CurvePreferences = %v, want %v", tlsCfg.CurvePreferences, want)
		}
	}

	t.Run("enrolled uplink", func(t *testing.T) {
		p := PKI{Dir: t.TempDir()}
		for name, data := range map[string][]byte{
			certFile: certPEM,
			keyFile:  keyPEM,
			caFile:   certPEM,
		} {
			if err := os.WriteFile(filepath.Join(p.Dir, name), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		tlsCfg, err := p.ClientTLS(cfg)
		if err != nil {
			t.Fatal(err)
		}
		assertPolicy(t, tlsCfg)
	})

	t.Run("CA bootstrap", func(t *testing.T) {
		caPath := filepath.Join(t.TempDir(), "ca.pem")
		if err := os.WriteFile(caPath, certPEM, 0o600); err != nil {
			t.Fatal(err)
		}
		tlsCfg, err := bootstrapTLS(cfg, Options{CACertFile: caPath})
		if err != nil {
			t.Fatal(err)
		}
		assertPolicy(t, tlsCfg)
	})

	t.Run("fingerprint bootstrap", func(t *testing.T) {
		tlsCfg, err := bootstrapTLS(cfg, Options{Fingerprint: "sha256:" + strings.Repeat("00", 32)})
		if err != nil {
			t.Fatal(err)
		}
		assertPolicy(t, tlsCfg)
	})
}

func TestHybridCurvePreferencesOverrideGODEBUGOptOuts(t *testing.T) {
	t.Setenv("GODEBUG", "tlsmlkem=0,tlssecpmlkem=0")
	serverCert, certPEM, _ := genServerCertificate(t, "grpc.test")
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	clientCfg, err := bootstrapTLS(
		config.Config{Server: config.Server{Address: "grpc.test:443"}},
		Options{CACertFile: caPath},
	)
	if err != nil {
		t.Fatal(err)
	}
	serverCfg := &tls.Config{
		Certificates:     []tls.Certificate{serverCert},
		MinVersion:       tls.VersionTLS13,
		CurvePreferences: hybridCurvePreferences(),
	}

	serverRaw, clientRaw := net.Pipe()
	serverConn := tls.Server(serverRaw, serverCfg)
	clientConn := tls.Client(clientRaw, clientCfg)
	t.Cleanup(func() {
		serverRaw.Close()
		clientRaw.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverErr := make(chan error, 1)
	go func() { serverErr <- serverConn.HandshakeContext(ctx) }()
	clientHandshakeErr := clientConn.HandshakeContext(ctx)
	serverHandshakeErr := <-serverErr
	if clientHandshakeErr != nil {
		t.Fatalf("client handshake: %v", clientHandshakeErr)
	}
	if serverHandshakeErr != nil {
		t.Fatalf("server handshake: %v", serverHandshakeErr)
	}
	if got := clientConn.ConnectionState().CurveID; got != tls.X25519MLKEM768 {
		t.Errorf("negotiated key exchange = %v, want X25519MLKEM768", got)
	}
}

func TestStageKeyAndCSRReusesValidPair(t *testing.T) {
	p := PKI{Dir: t.TempDir()}
	key1, csr1, err := p.stageKeyAndCSR()
	if err != nil {
		t.Fatal(err)
	}
	key2, csr2, err := p.stageKeyAndCSR()
	if err != nil {
		t.Fatal(err)
	}
	if !key1.Equal(key2) {
		t.Error("staged key not reused")
	}
	if string(csr1) != string(csr2) {
		t.Error("staged CSR not reused byte-identical (replay depends on it)")
	}
}

func TestStageKeyAndCSRRegeneratesCorruptCSR(t *testing.T) {
	p := PKI{Dir: t.TempDir()}
	key1, _, err := p.stageKeyAndCSR()
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash mid-write: readable but garbage CSR.
	if err := os.WriteFile(filepath.Join(p.Dir, csrStageFile), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	key2, csr2, err := p.stageKeyAndCSR()
	if err != nil {
		t.Fatal(err)
	}
	if key1.Equal(key2) {
		t.Error("corrupt staging state should regenerate the pair")
	}
	if _, err := x509.ParseCertificateRequest(csr2); err != nil {
		t.Errorf("regenerated CSR unparseable: %v", err)
	}
}

func TestStageKeyAndCSRRegeneratesMismatchedCSR(t *testing.T) {
	p := PKI{Dir: t.TempDir()}
	key1, _, err := p.stageKeyAndCSR()
	if err != nil {
		t.Fatal(err)
	}
	// A valid CSR made with a DIFFERENT key: accepting it would enroll a
	// certificate that does not match agent.key and wedge the agent.
	_, otherCSR := genKeyCSR(t)
	if err := os.WriteFile(filepath.Join(p.Dir, csrStageFile), otherCSR, 0o600); err != nil {
		t.Fatal(err)
	}
	key2, csr2, err := p.stageKeyAndCSR()
	if err != nil {
		t.Fatal(err)
	}
	if key1.Equal(key2) && string(csr2) == string(otherCSR) {
		t.Error("mismatched CSR accepted for staged key")
	}
	if csr, err := x509.ParseCertificateRequest(csr2); err != nil {
		t.Errorf("regenerated CSR unparseable: %v", err)
	} else if pub, ok := csr.PublicKey.(*ecdsa.PublicKey); !ok || !pub.Equal(&key2.PublicKey) {
		t.Error("regenerated CSR does not match regenerated key")
	}
}
