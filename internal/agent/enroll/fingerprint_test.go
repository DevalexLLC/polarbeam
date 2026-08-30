package enroll

// Direct tests for verifyByFingerprint — the callback that REPLACES
// standard TLS verification on the --fingerprint enrollment path. It is the
// trust decision for the whole bootstrap, so its acceptance and every
// rejection reason are pinned here.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"
)

// makeChain issues hostname's server leaf from a fresh CA and returns both
// raw certs plus the CA's fingerprint (the value `token create` prints).
func makeChain(t *testing.T, hostname string, caNotAfter time.Time) (leafDER, caDER []byte, caFP [32]byte) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "polarbeam test ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              caNotAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err = x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: hostname},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		DNSNames:     []string{hostname},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err = x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return leafDER, caDER, sha256.Sum256(caDER)
}

func TestVerifyByFingerprint(t *testing.T) {
	future := time.Now().Add(24 * time.Hour)
	leaf, ca, fp := makeChain(t, "grpc.test", future)

	// The good path: pin the CA, verify the presented [leaf, ca] chain.
	if err := verifyByFingerprint([][]byte{leaf, ca}, fp, "grpc.test"); err != nil {
		t.Errorf("valid pinned chain rejected: %v", err)
	}

	// The pin IS the trust anchor: an unrelated chain (even a perfectly
	// valid one) must be rejected when no presented cert matches it.
	otherLeaf, otherCA, _ := makeChain(t, "grpc.test", future)
	if err := verifyByFingerprint([][]byte{otherLeaf, otherCA}, fp, "grpc.test"); err == nil {
		t.Error("chain without the pinned certificate accepted")
	}

	// Hostname is verified, not skipped: the same pinned chain presented
	// for a different name fails.
	if err := verifyByFingerprint([][]byte{leaf, ca}, fp, "evil.test"); err == nil {
		t.Error("pinned chain accepted for the wrong hostname")
	}

	// Pinning the CA does not bless a leaf it never signed: pinned CA
	// present in the chain, but the leaf is from another CA.
	if err := verifyByFingerprint([][]byte{otherLeaf, ca}, fp, "grpc.test"); err == nil {
		t.Error("foreign leaf accepted because the pinned CA rode along")
	}

	// Expired pinned CA: full verification still applies.
	expLeaf, expCA, expFP := makeChain(t, "grpc.test", time.Now().Add(-time.Minute))
	if err := verifyByFingerprint([][]byte{expLeaf, expCA}, expFP, "grpc.test"); err == nil {
		t.Error("chain under an expired pinned CA accepted")
	}

	// Garbage bytes fail parsing, loudly.
	if err := verifyByFingerprint([][]byte{{0xde, 0xad}}, fp, "grpc.test"); err == nil {
		t.Error("unparsable certificate accepted")
	}
}
