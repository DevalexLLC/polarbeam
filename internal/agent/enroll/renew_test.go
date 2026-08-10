package enroll

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// enrolledPKI builds an enrolled-looking state dir: a hand-rolled CA plus a
// leaf issued to the agent key (the server ca package is deliberately not
// imported into the agent tree).
func enrolledPKI(t *testing.T, validity time.Duration) (PKI, *ecdsa.PrivateKey, *x509.Certificate) {
	t.Helper()
	p := PKI{Dir: t.TempDir()}

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}

	agentKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafDER := issueLeaf(t, caCert, caKey, &agentKey.PublicKey, validity)

	keyDER, err := x509.MarshalECPrivateKey(agentKey)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string, block *pem.Block, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(p.Dir, name), pem.EncodeToMemory(block), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(keyFile, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}, 0o600)
	write(caFile, &pem.Block{Type: "CERTIFICATE", Bytes: caDER}, 0o644)
	write(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: leafDER}, 0o644)

	leaf, err := x509.ParseCertificate(leafDER)
	if err != nil {
		t.Fatal(err)
	}
	return p, caKey, leaf
}

func issueLeaf(t *testing.T, caCert *x509.Certificate, caKey *ecdsa.PrivateKey, pub *ecdsa.PublicKey, validity time.Duration) []byte {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "agent"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, pub, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func TestLeafParsesCurrentCert(t *testing.T) {
	p, _, want := enrolledPKI(t, time.Hour)
	leaf, err := p.Leaf()
	if err != nil {
		t.Fatal(err)
	}
	if !leaf.Equal(want) {
		t.Error("Leaf returned a different certificate than on disk")
	}
}

func TestLeafUnenrolledNamesRemedy(t *testing.T) {
	p := PKI{Dir: t.TempDir()}
	if _, err := p.Leaf(); err == nil {
		t.Fatal("Leaf succeeded with no certificate")
	}
}

func TestRenewalCSRUsesExistingKey(t *testing.T) {
	p, _, _ := enrolledPKI(t, time.Hour)
	csrDER, err := p.RenewalCSR()
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		t.Fatal(err)
	}
	if err := csr.CheckSignature(); err != nil {
		t.Fatalf("CSR signature invalid: %v", err)
	}
	leaf, err := p.Leaf()
	if err != nil {
		t.Fatal(err)
	}
	if !csr.PublicKey.(*ecdsa.PublicKey).Equal(leaf.PublicKey) {
		t.Error("renewal CSR does not use the enrolled key — commit would produce a mismatched pair")
	}
}

func TestCommitRenewalSwapsCertOnly(t *testing.T) {
	p, caKey, oldLeaf := enrolledPKI(t, time.Hour)
	keyBefore, err := os.ReadFile(filepath.Join(p.Dir, keyFile))
	if err != nil {
		t.Fatal(err)
	}
	caBefore, err := os.ReadFile(filepath.Join(p.Dir, caFile))
	if err != nil {
		t.Fatal(err)
	}

	// Renewed leaf for the SAME key, longer validity.
	caPEM, _ := pem.Decode(caBefore)
	caCert, err := x509.ParseCertificate(caPEM.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	newDER := issueLeaf(t, caCert, caKey, oldLeaf.PublicKey.(*ecdsa.PublicKey), 2*time.Hour)

	if err := p.CommitRenewal(newDER, caPEM.Bytes); err != nil {
		t.Fatal(err)
	}

	leaf, err := p.Leaf()
	if err != nil {
		t.Fatal(err)
	}
	if leaf.Equal(oldLeaf) {
		t.Error("certificate not replaced")
	}
	keyAfter, _ := os.ReadFile(filepath.Join(p.Dir, keyFile))
	if string(keyBefore) != string(keyAfter) {
		t.Error("renewal must never touch the private key")
	}
	caAfter, _ := os.ReadFile(filepath.Join(p.Dir, caFile))
	if string(caBefore) != string(caAfter) {
		t.Error("identical CA bundle rewrite changed bytes")
	}

	// The renewed pair must still load as a matching keypair, and modes
	// must be preserved.
	if _, err := p.RenewalCSR(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(p.Dir, certFile))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("agent.crt mode = %o, want 644", fi.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(p.Dir, certFile+".tmp")); !os.IsNotExist(err) {
		t.Error("commit left a .tmp file behind")
	}
}

func TestCommitRenewalEmptyBundleKeepsCA(t *testing.T) {
	p, caKey, oldLeaf := enrolledPKI(t, time.Hour)
	caBefore, _ := os.ReadFile(filepath.Join(p.Dir, caFile))
	caPEM, _ := pem.Decode(caBefore)
	caCert, _ := x509.ParseCertificate(caPEM.Bytes)
	newDER := issueLeaf(t, caCert, caKey, oldLeaf.PublicKey.(*ecdsa.PublicKey), 2*time.Hour)

	if err := p.CommitRenewal(newDER, nil); err != nil {
		t.Fatal(err)
	}
	caAfter, _ := os.ReadFile(filepath.Join(p.Dir, caFile))
	if string(caBefore) != string(caAfter) {
		t.Error("empty bundle must leave ca.crt untouched")
	}
}
