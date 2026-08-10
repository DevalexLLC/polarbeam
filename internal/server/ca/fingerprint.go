package ca

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
)

// CertFingerprint returns the lowercase hex sha256 of a certificate's DER.
// Agents pin this at enrollment (`enroll --fingerprint`).
func CertFingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}
