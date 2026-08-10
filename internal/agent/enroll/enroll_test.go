package enroll

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
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
