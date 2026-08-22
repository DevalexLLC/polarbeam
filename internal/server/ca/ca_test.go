package ca

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

var testAlgorithms = []Algorithm{AlgMLDSA65, AlgECDSAP256}

func initAndLoad(t *testing.T) *CA {
	t.Helper()
	return initAndLoadAlg(t, DefaultAlgorithm)
}

func initAndLoadAlg(t *testing.T, alg Algorithm) *CA {
	t.Helper()
	dir := t.TempDir()
	if err := Init(dir, alg, false); err != nil {
		t.Fatal(err)
	}
	c, err := Load(dir, Lifetimes{})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// genCSR returns a CSR DER for a fresh key of the given algorithm.
func genCSR(t *testing.T, alg Algorithm) []byte {
	t.Helper()
	key, err := generateKey(alg)
	if err != nil {
		t.Fatal(err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}
	return csrDER
}

func TestParseAlgorithm(t *testing.T) {
	if alg, err := ParseAlgorithm(""); err != nil || alg != DefaultAlgorithm {
		t.Errorf("ParseAlgorithm(\"\") = %q, %v; want default %q", alg, err, DefaultAlgorithm)
	}
	for _, alg := range testAlgorithms {
		if got, err := ParseAlgorithm(string(alg)); err != nil || got != alg {
			t.Errorf("ParseAlgorithm(%q) = %q, %v", alg, got, err)
		}
	}
	if _, err := ParseAlgorithm("rsa-4096"); err == nil {
		t.Error("unknown algorithm accepted")
	}
}

func TestInitDefaultIsMLDSA65(t *testing.T) {
	c := initAndLoad(t)
	if c.cert.PublicKeyAlgorithm != x509.MLDSA {
		t.Errorf("root public key algorithm = %v, want ML-DSA", c.cert.PublicKeyAlgorithm)
	}
	if c.cert.SignatureAlgorithm != x509.MLDSA65 {
		t.Errorf("root signature algorithm = %v, want ML-DSA-65", c.cert.SignatureAlgorithm)
	}
	if got := c.Algorithm(); got != AlgMLDSA65 {
		t.Errorf("Algorithm() = %q, want %q", got, AlgMLDSA65)
	}
}

func TestInitRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	if err := Init(dir, DefaultAlgorithm, false); err != nil {
		t.Fatal(err)
	}
	if err := Init(dir, DefaultAlgorithm, false); err == nil {
		t.Fatal("second init overwrote the CA")
	}
	if err := Init(dir, DefaultAlgorithm, true); err != nil {
		t.Fatalf("init --if-missing should be a no-op, got: %v", err)
	}
	// --if-missing is a no-op even when the requested algorithm differs
	// from the existing CA's; the caller reports the loaded algorithm.
	if err := Init(dir, AlgECDSAP256, true); err != nil {
		t.Fatalf("init --if-missing with a different algorithm should be a no-op, got: %v", err)
	}
}

func TestLoadMissingNamesRemedy(t *testing.T) {
	_, err := Load(t.TempDir(), Lifetimes{})
	if err == nil || !strings.Contains(err.Error(), "ca init") {
		t.Fatalf("missing CA error should name the remedy: %v", err)
	}
}

// TestLoadLegacySEC1Key covers CAs created before the ML-DSA default, whose
// key is a SEC1 "EC PRIVATE KEY" block rather than PKCS#8.
func TestLoadLegacySEC1Key(t *testing.T) {
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "PolarBEAM CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(rootLifetime),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePEM(filepath.Join(dir, keyFile), "EC PRIVATE KEY", keyDER, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writePEM(filepath.Join(dir, certFile), "CERTIFICATE", der, 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := Load(dir, Lifetimes{})
	if err != nil {
		t.Fatalf("legacy SEC1 CA failed to load: %v", err)
	}
	if got := c.Algorithm(); got != AlgECDSAP256 {
		t.Errorf("Algorithm() = %q, want %q", got, AlgECDSAP256)
	}
	if _, _, _, err := c.SignAgentCSR(genCSR(t, AlgECDSAP256), uuid.New(), "h"); err != nil {
		t.Fatalf("legacy CA cannot sign: %v", err)
	}
}

func TestSignAgentCSRRoundTrip(t *testing.T) {
	for _, alg := range testAlgorithms {
		t.Run(string(alg), func(t *testing.T) {
			c := initAndLoadAlg(t, alg)
			agentID := uuid.New()
			csrDER := genCSR(t, alg)

			der, serial, notAfter, err := c.SignAgentCSR(csrDER, agentID, "host1.example.com")
			if err != nil {
				t.Fatal(err)
			}
			cert, err := x509.ParseCertificate(der)
			if err != nil {
				t.Fatal(err)
			}
			if cert.SerialNumber.Cmp(serial) != 0 {
				t.Error("serial mismatch")
			}
			if cert.NotAfter.Sub(cert.NotBefore) < AgentCertLifetime {
				t.Errorf("lifetime too short: %v", notAfter)
			}

			// Identity round-trips through the URI SAN.
			got, err := AgentIDFromCert(cert)
			if err != nil {
				t.Fatal(err)
			}
			if got != agentID {
				t.Errorf("agent ID mismatch: got %s want %s", got, agentID)
			}

			// Verifies against the CA pool with client-auth usage.
			if _, err := cert.Verify(x509.VerifyOptions{
				Roots:     c.Pool(),
				KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
			}); err != nil {
				t.Fatalf("issued cert does not verify against CA: %v", err)
			}
		})
	}
}

func TestSignAgentCSRRejectsGarbage(t *testing.T) {
	c := initAndLoad(t)
	if _, _, _, err := c.SignAgentCSR([]byte("not a csr"), uuid.New(), "h"); err == nil {
		t.Fatal("garbage CSR accepted")
	}
}

// TestSignAgentCSRRejectsAlgorithmMismatch: a leaf key differing from the
// root's exact algorithm and strength would silently diverge from the
// operator's choice, so the CA refuses to sign it. Same-family weaker
// parameters matter too: ML-DSA-44 and ML-DSA-65 both report x509.MLDSA,
// and every ECDSA curve reports x509.ECDSA.
func TestSignAgentCSRRejectsAlgorithmMismatch(t *testing.T) {
	genCSRFromKey := func(t *testing.T, key crypto.Signer) []byte {
		t.Helper()
		csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
		if err != nil {
			t.Fatal(err)
		}
		return csrDER
	}
	mldsa44Key, err := mldsa.GenerateKey(mldsa.MLDSA44())
	if err != nil {
		t.Fatal(err)
	}
	p384Key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		caAlg  Algorithm
		csrDER func(t *testing.T) []byte
	}{
		{"ecdsa csr under mldsa ca", AlgMLDSA65, func(t *testing.T) []byte { return genCSR(t, AlgECDSAP256) }},
		{"mldsa csr under ecdsa ca", AlgECDSAP256, func(t *testing.T) []byte { return genCSR(t, AlgMLDSA65) }},
		{"mldsa44 csr under mldsa65 ca", AlgMLDSA65, func(t *testing.T) []byte { return genCSRFromKey(t, mldsa44Key) }},
		{"p384 csr under p256 ca", AlgECDSAP256, func(t *testing.T) []byte { return genCSRFromKey(t, p384Key) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := initAndLoadAlg(t, tc.caAlg)
			_, _, _, err := c.SignAgentCSR(tc.csrDER(t), uuid.New(), "h")
			if err == nil || !strings.Contains(err.Error(), "does not match the CA algorithm") {
				t.Fatalf("mismatched CSR: err = %v, want algorithm-mismatch rejection", err)
			}
		})
	}
}

func TestAgentIDFromCertRejectsNoSAN(t *testing.T) {
	c := initAndLoad(t)
	certPEM, _, err := c.IssueServerCert("grpc.example.com")
	if err != nil {
		t.Fatal(err)
	}
	_ = certPEM
	if _, err := AgentIDFromCert(c.cert); err == nil {
		t.Fatal("CA cert accepted as agent cert")
	}
}

func TestIssueServerCert(t *testing.T) {
	for _, alg := range testAlgorithms {
		t.Run(string(alg), func(t *testing.T) {
			c := initAndLoadAlg(t, alg)
			certPEM, keyPEM, err := c.IssueServerCert("grpc.polarbeam.local")
			if err != nil {
				t.Fatal(err)
			}
			if len(keyPEM) == 0 {
				t.Fatal("no key")
			}
			// The PEM must contain the full chain (leaf first, then the CA):
			// fingerprint-pinned enrollment finds the CA among presented certs.
			certs, err := parseAllPEMCerts(certPEM)
			if err != nil || len(certs) != 2 {
				t.Fatalf("expected leaf+CA chain, got %d certs (err %v)", len(certs), err)
			}
			if !certs[1].Equal(c.cert) {
				t.Fatal("second chain element is not the CA certificate")
			}
			cert := certs[0]
			// The leaf key mirrors the root's algorithm.
			if cert.PublicKeyAlgorithm != c.cert.PublicKeyAlgorithm {
				t.Errorf("leaf public key algorithm = %v, want root's %v",
					cert.PublicKeyAlgorithm, c.cert.PublicKeyAlgorithm)
			}
			if err := cert.VerifyHostname("grpc.polarbeam.local"); err != nil {
				t.Fatal(err)
			}
			if _, err := cert.Verify(x509.VerifyOptions{
				Roots:     c.Pool(),
				DNSName:   "grpc.polarbeam.local",
				KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			}); err != nil {
				t.Fatalf("server cert does not verify: %v", err)
			}
		})
	}
}

func TestLifetimeOverrides(t *testing.T) {
	dir := t.TempDir()
	if err := Init(dir, DefaultAlgorithm, false); err != nil {
		t.Fatal(err)
	}
	c, err := Load(dir, Lifetimes{Agent: 10 * time.Minute, Server: 2 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	der, _, _, err := c.SignAgentCSR(genCSR(t, DefaultAlgorithm), uuid.New(), "h")
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	// NotBefore is backdated 5 minutes; validity = backdate + lifetime.
	if got := cert.NotAfter.Sub(cert.NotBefore); got != 15*time.Minute {
		t.Errorf("agent validity = %v, want 15m (5m backdate + 10m lifetime)", got)
	}

	certPEM, _, err := c.IssueServerCert("grpc.example.com")
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certPEM)
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if got := leaf.NotAfter.Sub(leaf.NotBefore); got != 2*time.Hour+5*time.Minute {
		t.Errorf("server validity = %v, want 2h5m", got)
	}
}

func TestZeroLifetimesFallBackToDefaults(t *testing.T) {
	c := initAndLoad(t) // Load with Lifetimes{}
	der, _, _, err := c.SignAgentCSR(genCSR(t, DefaultAlgorithm), uuid.New(), "h")
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	if got := cert.NotAfter.Sub(cert.NotBefore); got != AgentCertLifetime+5*time.Minute {
		t.Errorf("agent validity = %v, want default %v + 5m backdate", got, AgentCertLifetime)
	}
}
