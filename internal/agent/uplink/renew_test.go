package uplink

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/devalexllc/polarbeam/internal/agent/config"
	"github.com/devalexllc/polarbeam/internal/agent/enroll"
	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
)

func TestRenewAtFor(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		validity time.Duration
	}{
		{"30d production cert", 30 * 24 * time.Hour},
		{"15m test cert (10m lifetime + 5m backdate)", 15 * time.Minute},
	}
	for _, tt := range tests {
		leaf := &x509.Certificate{NotBefore: base, NotAfter: base.Add(tt.validity)}
		want := base.Add(tt.validity * 2 / 3)
		if got := renewAtFor(leaf); !got.Equal(want) {
			t.Errorf("%s: renewAt = %v, want %v (2/3 of validity)", tt.name, got, want)
		}
	}
}

func TestRetryIntervalFor(t *testing.T) {
	tests := []struct {
		validity time.Duration
		want     time.Duration
	}{
		{30 * 24 * time.Hour, 24 * time.Hour}, // production: retry daily (architecture.md)
		{15 * time.Minute, 45 * time.Second},  // 10m test mode: observable retries
		{2 * time.Minute, 30 * time.Second},   // floor
		{365 * 24 * time.Hour, 24 * time.Hour},
	}
	for _, tt := range tests {
		if got := retryIntervalFor(tt.validity); got != tt.want {
			t.Errorf("retryIntervalFor(%v) = %v, want %v", tt.validity, got, tt.want)
		}
	}
}

// testPKI builds an enrolled state dir (self-signed cert doubling as CA —
// the renewer never validates chains, only file plumbing matters here).
func testPKI(t *testing.T) (enroll.PKI, *ecdsa.PrivateKey, *x509.Certificate) {
	t.Helper()
	stateDir := t.TempDir()
	dir := filepath.Join(stateDir, "pki")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "agent"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	for name, blk := range map[string]*pem.Block{
		"agent.key": {Type: "EC PRIVATE KEY", Bytes: keyDER},
		"agent.crt": {Type: "CERTIFICATE", Bytes: der},
		"ca.crt":    {Type: "CERTIFICATE", Bytes: der},
	} {
		if err := os.WriteFile(filepath.Join(dir, name), pem.EncodeToMemory(blk), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return enroll.NewPKI(stateDir), key, cert
}

func TestRenewOnceSuccess(t *testing.T) {
	pki, key, old := testPKI(t)

	// The stub server issues a fresh cert for the CSR's key.
	newTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "agent"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(2 * time.Hour),
	}
	recycled := 0
	r := &Renewer{
		pki: pki,
		renew: func(_ context.Context, csrDER []byte) (*pb.RenewCertResponse, error) {
			csr, err := x509.ParseCertificateRequest(csrDER)
			if err != nil {
				t.Fatalf("renewer sent a bad CSR: %v", err)
			}
			der, err := x509.CreateCertificate(rand.Reader, newTmpl, old, csr.PublicKey, key)
			if err != nil {
				t.Fatal(err)
			}
			return &pb.RenewCertResponse{
				CertDer:     der,
				CaBundleDer: old.Raw,
				NotAfter:    timestamppb.New(newTmpl.NotAfter),
			}, nil
		},
		onRenewed: func() error { recycled++; return nil },
		now:       time.Now,
	}
	if err := r.renewOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if recycled != 1 {
		t.Errorf("onRenewed called %d times, want 1", recycled)
	}
	leaf, err := pki.Leaf()
	if err != nil {
		t.Fatal(err)
	}
	if leaf.SerialNumber.Cmp(newTmpl.SerialNumber) != 0 {
		t.Error("committed leaf is not the renewed certificate")
	}
}

func TestRenewOnceRPCFailureLeavesFilesUntouched(t *testing.T) {
	pki, _, old := testPKI(t)
	r := &Renewer{
		pki: pki,
		renew: func(context.Context, []byte) (*pb.RenewCertResponse, error) {
			return nil, errors.New("server unavailable")
		},
		onRenewed: func() error { t.Error("recycle must not run on failure"); return nil },
		now:       time.Now,
	}
	if err := r.renewOnce(context.Background()); err == nil {
		t.Fatal("renewOnce swallowed the RPC error")
	}
	leaf, err := pki.Leaf()
	if err != nil {
		t.Fatal(err)
	}
	if !leaf.Equal(old) {
		t.Error("failed renewal modified the on-disk certificate")
	}
}

func TestRenewOnceGarbageResponseFails(t *testing.T) {
	pki, _, old := testPKI(t)
	r := &Renewer{
		pki: pki,
		renew: func(context.Context, []byte) (*pb.RenewCertResponse, error) {
			return &pb.RenewCertResponse{CertDer: []byte("not a cert")}, nil
		},
		onRenewed: func() error { t.Error("recycle must not run on failure"); return nil },
		now:       time.Now,
	}
	// CommitRenewal writes PEM wrapping whatever DER it gets; Leaf then
	// fails to parse. renewOnce itself cannot detect this without parsing —
	// so it MUST parse before committing, or this test fails.
	if err := r.renewOnce(context.Background()); err == nil {
		t.Fatal("renewOnce committed an unparseable certificate")
	}
	leaf, err := pki.Leaf()
	if err != nil {
		t.Fatalf("on-disk certificate corrupted by rejected renewal: %v", err)
	}
	if !leaf.Equal(old) {
		t.Error("rejected renewal modified the on-disk certificate")
	}
}

func TestRecycleSwapIsRaceFree(t *testing.T) {
	pki, _, _ := testPKI(t)
	cfg := config.Defaults()
	cfg.StateDir = filepath.Dir(pki.Dir)
	cfg.Server.Address = "127.0.0.1:1" // grpc.NewClient is lazy; never dialed
	u, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			if err := u.Recycle(); err != nil {
				t.Errorf("recycle: %v", err)
				return
			}
		}
	}()
	for i := 0; i < 500; i++ {
		if u.getConn() == nil {
			t.Fatal("getConn returned nil during recycle")
		}
	}
	<-done
}
