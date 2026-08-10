package server

import (
	"crypto/x509"
	"testing"
	"time"

	"github.com/devalexllc/polarbeam/internal/server/ca"
)

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
