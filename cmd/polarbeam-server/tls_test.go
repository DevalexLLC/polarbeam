package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// genPair returns a self-signed ECDSA leaf and its key as PEM, with
// notAfter relative to now so expiry can be tested.
func genPair(t *testing.T, cn string, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		DNSNames:     []string{cn},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}

// tempLeftovers lists staged temp files left in dir.
func tempLeftovers(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, ".*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

type fixture struct {
	in, dst         string
	certIn, keyIn   string
	certDst, keyDst string
	certPEM, keyPEM []byte
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	root := t.TempDir()
	f := fixture{in: filepath.Join(root, "in"), dst: filepath.Join(root, "tls")}
	if err := os.MkdirAll(f.in, 0o755); err != nil {
		t.Fatal(err)
	}
	f.certPEM, f.keyPEM = genPair(t, "dashboard.example", time.Now().Add(365*24*time.Hour))
	f.certIn = filepath.Join(f.in, "cert.pem")
	f.keyIn = filepath.Join(f.in, "key.pem")
	writeFile(t, f.certIn, f.certPEM)
	writeFile(t, f.keyIn, f.keyPEM)
	f.certDst = filepath.Join(f.dst, "server.crt")
	f.keyDst = filepath.Join(f.dst, "server.key")
	return f
}

func TestInstallTLSSuccess(t *testing.T) {
	f := newFixture(t)
	summary, err := installTLS(f.certIn, f.keyIn, f.certDst, f.keyDst, 10001, false)
	if err != nil {
		t.Fatalf("installTLS: %v", err)
	}
	if !bytes.Equal(readFile(t, f.certDst), f.certPEM) || !bytes.Equal(readFile(t, f.keyDst), f.keyPEM) {
		t.Error("installed files differ from inputs")
	}
	if m := mode(t, f.certDst); m != 0o644 {
		t.Errorf("cert mode = %o, want 644", m)
	}
	if m := mode(t, f.keyDst); m != 0o600 {
		t.Errorf("key mode = %o, want 600", m)
	}
	for _, want := range []string{"dashboard.example", "127.0.0.1", "expires:", "chain:   1", "restart the server"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary lacks %q:\n%s", want, summary)
		}
	}
	if strings.Contains(summary, "owner uid") {
		t.Error("summary claims ownership was set although not running as root")
	}
	if left := tempLeftovers(t, f.dst); len(left) != 0 {
		t.Errorf("staged temp files left behind: %v", left)
	}
}

func TestInstallTLSOverwritesExistingPair(t *testing.T) {
	f := newFixture(t)
	if err := os.MkdirAll(f.dst, 0o755); err != nil {
		t.Fatal(err)
	}
	oldCert, oldKey := genPair(t, "old.example", time.Now().Add(time.Hour))
	writeFile(t, f.certDst, oldCert)
	writeFile(t, f.keyDst, oldKey)
	if _, err := installTLS(f.certIn, f.keyIn, f.certDst, f.keyDst, 10001, false); err != nil {
		t.Fatalf("installTLS: %v", err)
	}
	if !bytes.Equal(readFile(t, f.certDst), f.certPEM) || !bytes.Equal(readFile(t, f.keyDst), f.keyPEM) {
		t.Error("existing pair was not replaced")
	}
	if left := tempLeftovers(t, f.dst); len(left) != 0 {
		t.Errorf("staged temp files left behind: %v", left)
	}
}

func TestInstallTLSRejectsMismatch(t *testing.T) {
	f := newFixture(t)
	_, otherKey := genPair(t, "other.example", time.Now().Add(time.Hour))
	writeFile(t, f.keyIn, otherKey)
	_, err := installTLS(f.certIn, f.keyIn, f.certDst, f.keyDst, 10001, false)
	if err == nil {
		t.Fatal("mismatched pair: want error, got nil")
	}
	if !strings.Contains(err.Error(), "pair") {
		t.Errorf("error should say the pair is invalid, got %v", err)
	}
	if _, err := os.Stat(f.dst); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("destination dir should not be created on a rejected pair, stat err = %v", err)
	}
}

func TestInstallTLSMissingInputNamesPath(t *testing.T) {
	f := newFixture(t)
	missing := filepath.Join(f.in, "nope.pem")
	_, err := installTLS(missing, f.keyIn, f.certDst, f.keyDst, 10001, false)
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Errorf("missing cert: error should name %s, got %v", missing, err)
	}
	_, err = installTLS(f.certIn, missing, f.certDst, f.keyDst, 10001, false)
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Errorf("missing key: error should name %s, got %v", missing, err)
	}
}

func TestInstallTLSRefusesExpired(t *testing.T) {
	f := newFixture(t)
	certPEM, keyPEM := genPair(t, "expired.example", time.Now().Add(-time.Minute))
	writeFile(t, f.certIn, certPEM)
	writeFile(t, f.keyIn, keyPEM)
	_, err := installTLS(f.certIn, f.keyIn, f.certDst, f.keyDst, 10001, false)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("expired certificate: want error naming expiry, got %v", err)
	}
}

func TestInstallTLSRejectsSamePath(t *testing.T) {
	f := newFixture(t)
	_, err := installTLS(f.certIn, f.keyIn, f.certDst, f.certDst+"/../server.crt", 10001, false)
	if err == nil || !strings.Contains(err.Error(), "same path") {
		t.Errorf("same destination for cert and key: want error, got %v", err)
	}
}

// A phase-1 failure (here: an unwritable destination directory) must leave
// the existing pair byte-identical and no staged files behind.
func TestInstallTLSStageFailureKeepsOldPair(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	f := newFixture(t)
	if err := os.MkdirAll(f.dst, 0o755); err != nil {
		t.Fatal(err)
	}
	oldCert, oldKey := genPair(t, "old.example", time.Now().Add(time.Hour))
	writeFile(t, f.certDst, oldCert)
	writeFile(t, f.keyDst, oldKey)
	if err := os.Chmod(f.dst, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(f.dst, 0o755) })

	_, err := installTLS(f.certIn, f.keyIn, f.certDst, f.keyDst, 10001, false)
	if err == nil {
		t.Fatal("unwritable destination: want error, got nil")
	}
	if !strings.Contains(err.Error(), "existing pair untouched") {
		t.Errorf("error should state the old pair is untouched, got %v", err)
	}
	if !bytes.Equal(readFile(t, f.certDst), oldCert) || !bytes.Equal(readFile(t, f.keyDst), oldKey) {
		t.Error("existing pair was modified by a failed install")
	}
	if left := tempLeftovers(t, f.dst); len(left) != 0 {
		t.Errorf("staged temp files left behind: %v", left)
	}
}

// The one failure that cannot be rolled back: the second rename. The key
// is installed, the certificate is not, and the error must say so and
// name the staged certificate that was left behind.
func TestInstallTLSSecondRenameFailureIsLoud(t *testing.T) {
	f := newFixture(t)
	if err := os.MkdirAll(f.dst, 0o755); err != nil {
		t.Fatal(err)
	}
	oldCert, oldKey := genPair(t, "old.example", time.Now().Add(time.Hour))
	writeFile(t, f.certDst, oldCert)
	writeFile(t, f.keyDst, oldKey)

	calls := 0
	renameFile = func(from, to string) error {
		calls++
		if calls == 2 {
			return errors.New("injected rename failure")
		}
		return os.Rename(from, to)
	}
	t.Cleanup(func() { renameFile = os.Rename })

	_, err := installTLS(f.certIn, f.keyIn, f.certDst, f.keyDst, 10001, false)
	if err == nil {
		t.Fatal("second rename failure: want error, got nil")
	}
	for _, want := range []string{"MISMATCHED", f.keyDst, f.certDst, "staged certificate"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if !bytes.Equal(readFile(t, f.keyDst), f.keyPEM) {
		t.Error("new key should have been installed by the first rename")
	}
	if !bytes.Equal(readFile(t, f.certDst), oldCert) {
		t.Error("old certificate should still be in place")
	}
	left := tempLeftovers(t, f.dst)
	if len(left) != 1 || !bytes.Equal(readFile(t, left[0]), f.certPEM) {
		t.Errorf("exactly the staged certificate should remain, got %v", left)
	}
	if !strings.Contains(err.Error(), left[0]) {
		t.Errorf("error should name the staged file %s: %v", left[0], err)
	}
}

func TestTLSInstallProblems(t *testing.T) {
	if got := tlsInstallProblems("c", "k", 10001); got != nil {
		t.Errorf("valid flags: got problems %v", got)
	}
	got := tlsInstallProblems("", "", -1)
	if len(got) != 3 {
		t.Errorf("want three problems, got %v", got)
	}
}
