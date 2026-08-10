package probes

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
	"strings"
	"testing"
	"time"
)

// writePKI creates <stateDir>/pki with a self-signed cert of the given
// validity window and a key with keyMode.
func writePKI(t *testing.T, notBefore, notAfter time.Time, keyMode os.FileMode) string {
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
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "agent"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent.crt"),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent.key"),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), keyMode); err != nil {
		t.Fatal(err)
	}
	return stateDir
}

func byName(t *testing.T, checks []Check, name string) Check {
	t.Helper()
	for _, c := range checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %q check in %+v", name, checks)
	return Check{}
}

func TestIdentityUnenrolledIsInformational(t *testing.T) {
	checks := identityChecks(t.TempDir(), time.Now())
	if len(checks) != 1 {
		t.Fatalf("unenrolled should yield exactly the identity check, got %+v", checks)
	}
	c := checks[0]
	if !c.OK || c.Fatal {
		t.Errorf("not-enrolled must not block ExecStartPre before first enroll: %+v", c)
	}
	if !strings.Contains(c.Detail, "enroll") {
		t.Errorf("detail should name the remedy: %q", c.Detail)
	}
}

func TestIdentityValid(t *testing.T) {
	now := time.Now()
	dir := writePKI(t, now.Add(-time.Hour), now.Add(29*24*time.Hour), 0o600)
	checks := identityChecks(dir, now)
	id := byName(t, checks, "identity")
	if !id.OK || id.Fatal && !id.OK {
		t.Errorf("valid cert flagged: %+v", id)
	}
	if !strings.Contains(id.Detail, "valid until") {
		t.Errorf("detail should report expiry: %q", id.Detail)
	}
	perm := byName(t, checks, "pki permissions")
	if !perm.OK {
		t.Errorf("0600 key flagged: %+v", perm)
	}
}

func TestIdentityExpiringWarnsNonFatal(t *testing.T) {
	now := time.Now()
	// 30d validity with 9d left = inside the final third (renewal at 2/3
	// means it has been failing for ~a day).
	dir := writePKI(t, now.Add(-21*24*time.Hour), now.Add(9*24*time.Hour), 0o600)
	id := byName(t, identityChecks(dir, now), "identity")
	if id.OK || id.Fatal {
		t.Errorf("expiring cert should FAIL non-fatally: %+v", id)
	}
	if !strings.Contains(id.Detail, "renewal") {
		t.Errorf("detail should point at renewal: %q", id.Detail)
	}
}

func TestIdentityExpiredIsFatal(t *testing.T) {
	now := time.Now()
	dir := writePKI(t, now.Add(-31*24*time.Hour), now.Add(-24*time.Hour), 0o600)
	id := byName(t, identityChecks(dir, now), "identity")
	if id.OK || !id.Fatal {
		t.Errorf("expired cert must be fatal: %+v", id)
	}
	if !strings.Contains(id.Detail, "re-enroll") {
		t.Errorf("detail should name the remedy: %q", id.Detail)
	}
}

func TestPKIPermissionsWorldReadableKeyIsFatal(t *testing.T) {
	now := time.Now()
	dir := writePKI(t, now.Add(-time.Hour), now.Add(24*time.Hour), 0o644)
	perm := byName(t, identityChecks(dir, now), "pki permissions")
	if perm.OK || !perm.Fatal {
		t.Errorf("world-readable key must be fatal: %+v", perm)
	}
	if !strings.Contains(perm.Detail, "600") {
		t.Errorf("detail should name the fix: %q", perm.Detail)
	}
}

func TestSpoolCheckWritable(t *testing.T) {
	c := spoolCheck(t.TempDir())
	if !c.OK || !c.Fatal {
		t.Errorf("writable spool: %+v", c)
	}
	if _, err := os.Stat(filepath.Join(c.Detail)); err != nil && !strings.Contains(c.Detail, "writable") {
		t.Errorf("detail should name the dir: %q", c.Detail)
	}
}

func TestSpoolCheckUnwritableIsFatal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory modes")
	}
	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(stateDir, 0o700) })
	c := spoolCheck(stateDir)
	if c.OK || !c.Fatal {
		t.Errorf("unwritable spool dir must be fatal: %+v", c)
	}
}
