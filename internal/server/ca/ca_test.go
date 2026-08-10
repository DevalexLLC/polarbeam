package ca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func initAndLoad(t *testing.T) *CA {
	t.Helper()
	dir := t.TempDir()
	if err := Init(dir, false); err != nil {
		t.Fatal(err)
	}
	c, err := Load(dir, Lifetimes{})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestInitRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	if err := Init(dir, false); err != nil {
		t.Fatal(err)
	}
	if err := Init(dir, false); err == nil {
		t.Fatal("second init overwrote the CA")
	}
	if err := Init(dir, true); err != nil {
		t.Fatalf("init --if-missing should be a no-op, got: %v", err)
	}
}

func TestLoadMissingNamesRemedy(t *testing.T) {
	_, err := Load(t.TempDir(), Lifetimes{})
	if err == nil || !strings.Contains(err.Error(), "ca init") {
		t.Fatalf("missing CA error should name the remedy: %v", err)
	}
}

func TestSignAgentCSRRoundTrip(t *testing.T) {
	c := initAndLoad(t)
	agentID := uuid.New()

	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	csrDER, err := x509.CreateCertificateRequest(rand.Reader,
		&x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}

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
}

func TestSignAgentCSRRejectsGarbage(t *testing.T) {
	c := initAndLoad(t)
	if _, _, _, err := c.SignAgentCSR([]byte("not a csr"), uuid.New(), "h"); err == nil {
		t.Fatal("garbage CSR accepted")
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
	c := initAndLoad(t)
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
}

func TestLifetimeOverrides(t *testing.T) {
	dir := t.TempDir()
	if err := Init(dir, false); err != nil {
		t.Fatal(err)
	}
	c, err := Load(dir, Lifetimes{Agent: 10 * time.Minute, Server: 2 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}
	der, _, _, err := c.SignAgentCSR(csrDER, uuid.New(), "h")
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
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	csrDER, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	der, _, _, err := c.SignAgentCSR(csrDER, uuid.New(), "h")
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	if got := cert.NotAfter.Sub(cert.NotBefore); got != AgentCertLifetime+5*time.Minute {
		t.Errorf("agent validity = %v, want default %v + 5m backdate", got, AgentCertLifetime)
	}
}
