// Package ca implements the PolarBEAM built-in certificate authority.
//
// The CA signs two kinds of certificates:
//   - agent client certificates (identity in a URI SAN, polarbeam://agent/<uuid>)
//   - the server's own gRPC-facing certificate (SAN = listen.grpc_hostname),
//     auto-issued at startup so operators never manage the agent-facing cert.
//
// Private keys never leave their host: agents send CSRs, the CA returns
// certificates. Revocation is database-backed (the control plane is the only
// verifier), so no CRL/OCSP machinery exists here.
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
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
)

// Algorithm selects the CA key algorithm at `ca init`. The default is
// ML-DSA-65 (FIPS 204, NIST category 3) so agent and server identities
// resist a future quantum adversary; ecdsa-p256 remains as a classical
// escape hatch. Issued leaf keys mirror the root's algorithm.
type Algorithm string

const (
	AlgMLDSA65   Algorithm = "mldsa65"
	AlgECDSAP256 Algorithm = "ecdsa-p256"

	DefaultAlgorithm = AlgMLDSA65
)

// ParseAlgorithm maps operator input to an Algorithm; empty means the
// default.
func ParseAlgorithm(s string) (Algorithm, error) {
	switch Algorithm(s) {
	case "":
		return DefaultAlgorithm, nil
	case AlgMLDSA65, AlgECDSAP256:
		return Algorithm(s), nil
	default:
		return "", fmt.Errorf("unknown CA algorithm %q (supported: %s, %s)", s, AlgMLDSA65, AlgECDSAP256)
	}
}

const (
	keyFile  = "ca.key"
	certFile = "ca.crt"

	rootLifetime = 10 * 365 * 24 * time.Hour
	// AgentCertLifetime is the default issued client-certificate lifetime;
	// agents renew at 2/3 of the leaf's actual validity. An agent dark for
	// longer than the lifetime must re-enroll.
	AgentCertLifetime = 30 * 24 * time.Hour
	// ServerCertLifetime is the default lifetime of the auto-issued gRPC
	// server certificate, reissued when less than 1/3 remains.
	ServerCertLifetime = 90 * 24 * time.Hour
)

// Lifetimes overrides the issued-certificate lifetimes. Zero values fall
// back to the package defaults, so `Lifetimes{}` is always safe.
type Lifetimes struct {
	Agent  time.Duration
	Server time.Duration
}

func (l Lifetimes) agent() time.Duration {
	if l.Agent > 0 {
		return l.Agent
	}
	return AgentCertLifetime
}

func (l Lifetimes) server() time.Duration {
	if l.Server > 0 {
		return l.Server
	}
	return ServerCertLifetime
}

// AgentURISAN returns the URI SAN encoding an agent identity.
func AgentURISAN(agentID uuid.UUID) *url.URL {
	return &url.URL{Scheme: "polarbeam", Host: "agent", Path: "/" + agentID.String()}
}

// AgentIDFromCert extracts the agent identity from a client certificate's
// URI SAN. Fails if the SAN is absent or malformed.
func AgentIDFromCert(cert *x509.Certificate) (uuid.UUID, error) {
	for _, u := range cert.URIs {
		if u.Scheme == "polarbeam" && u.Host == "agent" && len(u.Path) > 1 {
			id, err := uuid.Parse(u.Path[1:])
			if err != nil {
				return uuid.Nil, fmt.Errorf("malformed agent URI SAN %q: %w", u, err)
			}
			return id, nil
		}
	}
	return uuid.Nil, errors.New("certificate has no polarbeam://agent/<uuid> URI SAN")
}

type CA struct {
	key       crypto.Signer
	cert      *x509.Certificate
	lifetimes Lifetimes
}

// Init creates a new CA in dir with the given key algorithm. It refuses to
// overwrite an existing CA; with ifMissing it succeeds as a no-op when one
// already exists, regardless of that CA's algorithm.
func Init(dir string, alg Algorithm, ifMissing bool) error {
	if _, err := os.Stat(filepath.Join(dir, keyFile)); err == nil {
		if ifMissing {
			return nil
		}
		return fmt.Errorf("CA already exists in %s (refusing to overwrite)", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("ca init: %w", err)
	}

	key, err := generateKey(alg)
	if err != nil {
		return fmt.Errorf("ca init: generate key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "PolarBEAM CA", Organization: []string{"PolarBEAM"}},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(rootLifetime),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return fmt.Errorf("ca init: self-sign: %w", err)
	}

	// New keys are always PKCS#8 ("PRIVATE KEY"), whatever the algorithm;
	// only reading stays dual-format for CAs created before the ML-DSA
	// default (SEC1 "EC PRIVATE KEY").
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("ca init: marshal key: %w", err)
	}
	if err := writePEM(filepath.Join(dir, keyFile), "PRIVATE KEY", keyDER, 0o600); err != nil {
		return err
	}
	return writePEM(filepath.Join(dir, certFile), "CERTIFICATE", der, 0o644)
}

func generateKey(alg Algorithm) (crypto.Signer, error) {
	switch alg {
	case AlgMLDSA65:
		return mldsa.GenerateKey(mldsa.MLDSA65())
	case AlgECDSAP256:
		return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	default:
		return nil, fmt.Errorf("unknown CA algorithm %q", alg)
	}
}

// Load reads the CA from dir. A missing CA is a preflight failure with the
// remedy named.
func Load(dir string, lifetimes Lifetimes) (*CA, error) {
	key, err := loadSigner(filepath.Join(dir, keyFile))
	if err != nil {
		return nil, fmt.Errorf("CA not usable in %s (run `polarbeam-server ca init` first): %w", dir, err)
	}
	certDER, err := readPEM(filepath.Join(dir, certFile), "CERTIFICATE")
	if err != nil {
		return nil, fmt.Errorf("CA not usable in %s: %w", dir, err)
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("parse CA certificate: %w", err)
	}
	return &CA{key: key, cert: cert, lifetimes: lifetimes}, nil
}

// loadSigner reads a private key from a PEM file: PKCS#8 ("PRIVATE KEY",
// what Init writes) or legacy SEC1 ("EC PRIVATE KEY", CAs created before
// the ML-DSA default).
func loadSigner(path string) (crypto.Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%s: expected a PRIVATE KEY or EC PRIVATE KEY PEM block", path)
	}
	switch block.Type {
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse CA key: %w", err)
		}
		key, ok := parsed.(crypto.Signer)
		if !ok {
			return nil, fmt.Errorf("parse CA key: %T cannot sign", parsed)
		}
		return key, nil
	case "EC PRIVATE KEY":
		key, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse CA key: %w", err)
		}
		return key, nil
	default:
		return nil, fmt.Errorf("%s: expected a PRIVATE KEY or EC PRIVATE KEY PEM block, got %s", path, block.Type)
	}
}

// Certificate returns the CA certificate.
func (c *CA) Certificate() *x509.Certificate { return c.cert }

// Algorithm returns the CA key algorithm, derived from the certificate.
func (c *CA) Algorithm() Algorithm {
	switch c.cert.PublicKeyAlgorithm {
	case x509.MLDSA:
		return AlgMLDSA65
	case x509.ECDSA:
		return AlgECDSAP256
	default:
		return Algorithm(c.cert.PublicKeyAlgorithm.String())
	}
}

// BundleDER returns the CA certificate in DER (what agents install as their
// trust anchor).
func (c *CA) BundleDER() []byte { return c.cert.Raw }

// Pool returns a cert pool containing the CA, for verifying client certs.
func (c *CA) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(c.cert)
	return p
}

// Fingerprint returns the sha256 of the CA certificate (hex), printed by
// `token create` so enrolling agents can pin trust without a file copy.
func (c *CA) Fingerprint() string {
	return CertFingerprint(c.cert)
}

// ValidateCSR checks that csrDER parses and is self-consistent, without
// issuing anything — callers reject bad CSRs before consuming join tokens.
func ValidateCSR(csrDER []byte) error {
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return fmt.Errorf("parse CSR: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return fmt.Errorf("CSR signature invalid: %w", err)
	}
	return nil
}

// SignAgentCSR validates csrDER and issues a client certificate binding the
// key to agentID via the URI SAN. Returns the certificate DER.
func (c *CA) SignAgentCSR(csrDER []byte, agentID uuid.UUID, hostname string) (der []byte, serial *big.Int, notAfter time.Time, err error) {
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("parse CSR: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("CSR signature invalid: %w", err)
	}
	// A leaf must not weaken the root's algorithm choice: a classical key
	// under an ML-DSA root would silently keep that identity's TLS
	// authentication classical (and vice versa breaks the escape hatch).
	// Shipped agents derive their key from the CA, so a mismatch is a
	// stale or foreign client.
	if !c.leafKeyAllowed(csr.PublicKey) {
		return nil, nil, time.Time{}, fmt.Errorf(
			"CSR key does not match the CA algorithm %s (agents derive their key algorithm from the CA at enrollment; upgrade or re-enroll the agent)",
			c.Algorithm())
	}
	serial, err = randomSerial()
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	now := time.Now()
	notAfter = now.Add(c.lifetimes.agent())
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: hostname, Organization: []string{"PolarBEAM Agent"}},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:         []*url.URL{AgentURISAN(agentID)},
	}
	der, err = x509.CreateCertificate(rand.Reader, tmpl, c.cert, csr.PublicKey, c.key)
	if err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("sign agent certificate: %w", err)
	}
	return der, serial, notAfter, nil
}

// IssueServerCert issues (or reissues) the gRPC-facing server certificate
// for hostname, generating a fresh key. The returned certPEM contains the
// full chain (leaf + CA): fingerprint-pinned enrollment verifies by finding
// the pinned CA among the certificates the server presents, so the CA must
// be served in the handshake.
func (c *CA) IssueServerCert(hostname string) (certPEM, keyPEM []byte, err error) {
	key, err := c.leafKey()
	if err != nil {
		return nil, nil, fmt.Errorf("issue server cert: generate key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: hostname, Organization: []string{"PolarBEAM Server"}},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.Add(c.lifetimes.server()),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{hostname},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, key.Public(), c.key)
	if err != nil {
		return nil, nil, fmt.Errorf("issue server cert: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("issue server cert: marshal key: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.cert.Raw})...)
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// leafKeyAllowed reports whether pub matches the root key's exact
// algorithm AND strength. x509.PublicKeyAlgorithm alone is not enough:
// all three ML-DSA parameter sets report x509.MLDSA, and every ECDSA
// curve reports x509.ECDSA — an ML-DSA-44 or P-384 leaf under an
// ML-DSA-65 or P-256 root would still silently diverge from the
// operator's choice.
func (c *CA) leafKeyAllowed(pub crypto.PublicKey) bool {
	switch root := c.cert.PublicKey.(type) {
	case *mldsa.PublicKey:
		leaf, ok := pub.(*mldsa.PublicKey)
		return ok && leaf.Parameters() == root.Parameters()
	case *ecdsa.PublicKey:
		leaf, ok := pub.(*ecdsa.PublicKey)
		return ok && leaf.Curve == root.Curve
	default:
		return false
	}
}

// leafKey generates a fresh leaf key with the same algorithm as the root,
// so issued identities never outlive the root algorithm's guarantees.
func (c *CA) leafKey() (crypto.Signer, error) {
	switch pub := c.cert.PublicKey.(type) {
	case *mldsa.PublicKey:
		return mldsa.GenerateKey(pub.Parameters())
	case *ecdsa.PublicKey:
		return ecdsa.GenerateKey(pub.Curve, rand.Reader)
	default:
		return nil, fmt.Errorf("unsupported CA public key type %T", pub)
	}
}

func randomSerial() (*big.Int, error) {
	// 128-bit random serial per CA/Browser Forum practice.
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	s, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}
	return s, nil
}

func writePEM(path, blockType string, der []byte, mode os.FileMode) error {
	data := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if err := os.WriteFile(path, data, mode); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func readPEM(path, wantType string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != wantType {
		return nil, fmt.Errorf("%s: expected a %s PEM block", path, wantType)
	}
	return block.Bytes, nil
}
