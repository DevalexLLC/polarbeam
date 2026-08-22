// Package enroll implements agent enrollment and PKI state.
//
// Trust bootstrap is explicit — there is no trust-on-first-use: the operator
// supplies either the CA certificate file (--ca-cert) or its sha256
// fingerprint (--fingerprint, printed by `polarbeam-server token create`).
package enroll

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/devalexllc/polarbeam/internal/agent/config"
	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
	"github.com/devalexllc/polarbeam/internal/version"
)

const (
	keyFile      = "agent.key"
	certFile     = "agent.crt"
	caFile       = "ca.crt"
	agentIDFile  = "agent-id"
	csrStageFile = "enroll.csr"
)

// PKI is the agent's on-disk identity state under <state_dir>/pki.
type PKI struct {
	Dir string
}

func NewPKI(stateDir string) PKI {
	return PKI{Dir: filepath.Join(stateDir, "pki")}
}

// Enrolled reports whether a certificate is present.
func (p PKI) Enrolled() bool {
	_, err := os.Stat(filepath.Join(p.Dir, certFile))
	return err == nil
}

// KeyPath returns the private-key location (selfcheck audits its mode).
func (p PKI) KeyPath() string { return filepath.Join(p.Dir, keyFile) }

// Options controls a single enrollment.
type Options struct {
	Token string
	// Exactly one of CACertFile / Fingerprint provides the trust anchor.
	CACertFile string
	// Fingerprint is "sha256:<hex>" of the CA certificate.
	Fingerprint string
	// ProbeAddress peers should target (required behind NAT).
	ProbeAddress string
}

// hybridCurvePreferences returns the agent's required key-exchange groups.
// Keeping them explicit makes the post-quantum transport policy independent
// of Go's process-wide ML-KEM defaults and their GODEBUG compatibility knobs.
func hybridCurvePreferences() []tls.CurveID {
	return []tls.CurveID{
		tls.X25519MLKEM768,
		tls.SecP256r1MLKEM768,
		tls.SecP384r1MLKEM1024,
	}
}

// Run enrolls against cfg.Server and writes the PKI state. Refuses to
// overwrite an existing enrollment.
func Run(ctx context.Context, cfg config.Config, opts Options) error {
	p := NewPKI(cfg.StateDir)
	if p.Enrolled() {
		return fmt.Errorf("already enrolled (%s exists); remove %s to re-enroll",
			filepath.Join(p.Dir, certFile), p.Dir)
	}
	if opts.Token == "" {
		return errors.New("--token is required")
	}
	tlsCfg, err := bootstrapTLS(cfg, opts)
	if err != nil {
		return err
	}
	// The agent's key algorithm mirrors the CA it was told to trust — the
	// server operator's choice at `ca init`, never the agent's.
	caCert, err := trustAnchor(ctx, cfg, opts, tlsCfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return fmt.Errorf("create pki dir: %w", err)
	}

	// Key and CSR are staged on disk BEFORE the RPC and reused on retry:
	// if the server commits the enrollment but the response is lost, the
	// retry presents the identical CSR and the server treats it as an
	// idempotent replay instead of a consumed token.
	key, csrDER, err := p.stageKeyAndCSR(caCert)
	if err != nil {
		return err
	}
	hostname, _ := os.Hostname()

	conn, err := grpc.NewClient(cfg.Server.Address,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	if err != nil {
		return fmt.Errorf("connect %s: %w", cfg.Server.Address, err)
	}
	defer conn.Close()

	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := pb.NewEnrollmentServiceClient(conn).Enroll(callCtx, &pb.EnrollRequest{
		JoinToken:    opts.Token,
		CsrDer:       csrDER,
		Hostname:     hostname,
		AgentVersion: version.Version,
		ProbeAddress: opts.ProbeAddress,
	})
	if err != nil {
		return fmt.Errorf("enrollment rejected by %s: %w", cfg.Server.Address, err)
	}

	if err := p.write(key, resp); err != nil {
		return err
	}
	os.Remove(filepath.Join(p.Dir, csrStageFile)) // staging no longer needed
	fmt.Printf("enrolled as agent %s (certificate valid until %s)\n",
		resp.GetAgentId(), resp.GetNotAfter().AsTime().Format(time.RFC3339))
	return nil
}

// newAgentKey generates a private key whose algorithm mirrors the CA's, so
// the escape-hatch choice made at `ca init` holds end to end.
func newAgentKey(caCert *x509.Certificate) (crypto.Signer, error) {
	switch pub := caCert.PublicKey.(type) {
	case *mldsa.PublicKey:
		return mldsa.GenerateKey(pub.Parameters())
	case *ecdsa.PublicKey:
		return ecdsa.GenerateKey(pub.Curve, rand.Reader)
	default:
		return nil, fmt.Errorf("unsupported CA public key algorithm %v; this agent supports ML-DSA and ECDSA",
			caCert.PublicKeyAlgorithm)
	}
}

// parsePrivateKeyPEM reads a PKCS#8 "PRIVATE KEY" block (what enrollment
// writes) or a legacy SEC1 "EC PRIVATE KEY" block (agents enrolled before
// the ML-DSA default).
func parsePrivateKeyPEM(keyPEM []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, errors.New("expected a PRIVATE KEY or EC PRIVATE KEY PEM block")
	}
	switch block.Type {
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		key, ok := parsed.(crypto.Signer)
		if !ok {
			return nil, fmt.Errorf("%T cannot sign", parsed)
		}
		return key, nil
	case "EC PRIVATE KEY":
		return x509.ParseECPrivateKey(block.Bytes)
	default:
		return nil, fmt.Errorf("expected a PRIVATE KEY or EC PRIVATE KEY PEM block, got %s", block.Type)
	}
}

// parseStagedPair returns the staged private key iff the key parses, the
// CSR parses with a valid signature, the CSR was made with that key, and
// the key's algorithm matches what the trusted CA calls for.
func parseStagedPair(keyPEM, csrDER []byte, want x509.PublicKeyAlgorithm) crypto.Signer {
	key, err := parsePrivateKeyPEM(keyPEM)
	if err != nil {
		return nil
	}
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil || csr.CheckSignature() != nil {
		return nil
	}
	if csr.PublicKeyAlgorithm != want {
		return nil
	}
	pub, ok := key.Public().(interface{ Equal(crypto.PublicKey) bool })
	if !ok || !pub.Equal(csr.PublicKey) {
		return nil
	}
	return key
}

// stageKeyAndCSR returns the staged key+CSR from an interrupted prior
// attempt, or generates and persists fresh ones.
func (p PKI) stageKeyAndCSR(caCert *x509.Certificate) (crypto.Signer, []byte, error) {
	keyPath := filepath.Join(p.Dir, keyFile)
	csrPath := filepath.Join(p.Dir, csrStageFile)

	keyPEM, keyErr := os.ReadFile(keyPath)
	csrDER, csrErr := os.ReadFile(csrPath)
	if keyErr == nil && csrErr == nil {
		if key := parseStagedPair(keyPEM, csrDER, caCert.PublicKeyAlgorithm); key != nil {
			return key, csrDER, nil
		}
		// Corrupt, mismatched, or wrong-algorithm staging state (crash
		// mid-write, stray files, a CA cut over between attempts):
		// regenerate both rather than retrying garbage forever or
		// enrolling a certificate that does not match our key.
	}

	key, err := newAgentKey(caCert)
	if err != nil {
		return nil, nil, fmt.Errorf("generate key: %w", err)
	}
	csrDER, err = x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create CSR: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal key: %w", err)
	}
	if err := os.WriteFile(keyPath,
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return nil, nil, fmt.Errorf("stage key: %w", err)
	}
	if err := os.WriteFile(csrPath, csrDER, 0o600); err != nil {
		return nil, nil, fmt.Errorf("stage CSR: %w", err)
	}
	return key, csrDER, nil
}

// write persists enrollment state. agent.crt is the "enrolled" marker
// (Enrolled checks it), so it is written LAST and atomically via rename: a
// failure part-way (full disk, crash) leaves no certificate file and the
// enrollment can simply be retried instead of wedging half-enrolled.
func (p PKI) write(key crypto.Signer, resp *pb.EnrollResponse) error {
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshal key: %w", err)
	}
	files := []struct {
		name string
		data []byte
		mode os.FileMode
	}{
		{keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600},
		{caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: resp.GetCaBundleDer()}), 0o644},
		{agentIDFile, []byte(resp.GetAgentId() + "\n"), 0o644},
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(p.Dir, f.name), f.data, f.mode); err != nil {
			return fmt.Errorf("write %s: %w", f.name, err)
		}
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: resp.GetCertDer()})
	tmp := filepath.Join(p.Dir, certFile+".tmp")
	if err := os.WriteFile(tmp, certPEM, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, filepath.Join(p.Dir, certFile)); err != nil {
		return fmt.Errorf("commit %s: %w", certFile, err)
	}
	return nil
}

// Leaf returns the current agent certificate.
func (p PKI) Leaf() (*x509.Certificate, error) {
	pemBytes, err := os.ReadFile(filepath.Join(p.Dir, certFile))
	if err != nil {
		return nil, fmt.Errorf("agent certificate unusable (re-enroll?): %w", err)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%s: expected a CERTIFICATE PEM block", filepath.Join(p.Dir, certFile))
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse agent certificate: %w", err)
	}
	return leaf, nil
}

// RenewalCSR builds a CSR from the EXISTING private key, in memory. Renewal
// deliberately does not rekey: the commit is then a single atomic swap of
// agent.crt with no key/cert-mismatch window (rekey-on-renew would need a
// two-file commit with crash recovery; revisit if key compromise rotation
// is ever needed — that path is re-enrollment today).
func (p PKI) RenewalCSR() ([]byte, error) {
	keyPEM, err := os.ReadFile(filepath.Join(p.Dir, keyFile))
	if err != nil {
		return nil, fmt.Errorf("agent key unusable (re-enroll?): %w", err)
	}
	key, err := parsePrivateKeyPEM(keyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse agent key %s: %w", filepath.Join(p.Dir, keyFile), err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		return nil, fmt.Errorf("create renewal CSR: %w", err)
	}
	return csrDER, nil
}

// CommitRenewal persists a renewed certificate. Both files go through
// tmp+rename; agent.crt is committed LAST (the same "cert last" invariant as
// enrollment) so a crash at any point leaves a matching, usable key/cert pair.
func (p PKI) CommitRenewal(certDER, caBundleDER []byte) error {
	writeAtomic := func(name string, data []byte, mode os.FileMode) error {
		tmp := filepath.Join(p.Dir, name+".tmp")
		if err := os.WriteFile(tmp, data, mode); err != nil {
			return fmt.Errorf("write %s: %w", tmp, err)
		}
		if err := os.Rename(tmp, filepath.Join(p.Dir, name)); err != nil {
			return fmt.Errorf("commit %s: %w", name, err)
		}
		return nil
	}
	if len(caBundleDER) > 0 {
		caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caBundleDER})
		if err := writeAtomic(caFile, caPEM, 0o644); err != nil {
			return err
		}
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	return writeAtomic(certFile, certPEM, 0o644)
}

// ClientTLS builds the mTLS config for the agent uplink from enrolled state.
// The eager load is a fail-fast check only; handshakes load the certificate
// from disk via GetClientCertificate so a renewal committed while the agent
// runs takes effect on the next handshake without a restart.
func (p PKI) ClientTLS(cfg config.Config) (*tls.Config, error) {
	certPath := filepath.Join(p.Dir, certFile)
	keyPath := filepath.Join(p.Dir, keyFile)
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		return nil, fmt.Errorf("agent certificate unusable (re-enroll?): %w", err)
	}
	caPEM, err := os.ReadFile(filepath.Join(p.Dir, caFile))
	if err != nil {
		return nil, fmt.Errorf("CA bundle unusable (re-enroll?): %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("CA bundle contains no certificates")
	}
	return &tls.Config{
		MinVersion:       tls.VersionTLS13,
		CurvePreferences: hybridCurvePreferences(),
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			cert, err := tls.LoadX509KeyPair(certPath, keyPath)
			if err != nil {
				return nil, fmt.Errorf("agent certificate unusable (re-enroll?): %w", err)
			}
			return &cert, nil
		},
		RootCAs:    pool,
		ServerName: serverName(cfg),
	}, nil
}

// trustAnchor returns the CA certificate the operator told the agent to
// trust: the --ca-cert file, or for --fingerprint the pinned chain member
// fetched over one preflight handshake. bootstrapTLS has already validated
// that exactly one of the two options is set.
func trustAnchor(ctx context.Context, cfg config.Config, opts Options, tlsCfg *tls.Config) (*x509.Certificate, error) {
	if opts.CACertFile != "" {
		data, err := os.ReadFile(opts.CACertFile)
		if err != nil {
			return nil, fmt.Errorf("--ca-cert: %w", err)
		}
		for block, rest := pem.Decode(data); block != nil; block, rest = pem.Decode(rest) {
			if block.Type != "CERTIFICATE" {
				continue
			}
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("--ca-cert %s: %w", opts.CACertFile, err)
			}
			return cert, nil
		}
		return nil, fmt.Errorf("--ca-cert %s contains no certificates", opts.CACertFile)
	}
	want, err := parseFingerprint(opts.Fingerprint)
	if err != nil {
		return nil, err
	}
	return fetchPinnedCA(ctx, cfg.Server.Address, tlsCfg, want)
}

// fetchPinnedCA dials once with the bootstrap config — whose verify callback
// enforces the pin — and returns the presented chain member matching the
// pinned fingerprint. The server always serves the CA in the handshake
// (IssueServerCert sends leaf+CA) precisely so pinned enrollment can find it.
func fetchPinnedCA(ctx context.Context, addr string, tlsCfg *tls.Config, want [32]byte) (*x509.Certificate, error) {
	// Bound the preflight like the enrollment RPC that follows it — the
	// caller's context typically has no deadline of its own.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cfg := tlsCfg.Clone()
	// Offer ALPN "h2" so this preflight is indistinguishable from the gRPC
	// dial that follows it — grpc-go servers may enforce ALPN, and a
	// middlebox should see no difference between the two connections.
	cfg.NextProtos = []string{"h2"}
	d := tls.Dialer{Config: cfg}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("fetch pinned CA certificate from %s: %w", addr, err)
	}
	defer conn.Close()
	for _, c := range conn.(*tls.Conn).ConnectionState().PeerCertificates {
		if sha256.Sum256(c.Raw) == want {
			return c, nil
		}
	}
	// Unreachable in practice: the verify callback already required the
	// pinned certificate in the chain for the handshake to succeed.
	return nil, errors.New("server did not present a certificate matching the pinned fingerprint")
}

// bootstrapTLS builds the server-verification config used before the agent
// trusts anything.
func bootstrapTLS(cfg config.Config, opts Options) (*tls.Config, error) {
	name := serverName(cfg)
	switch {
	case opts.CACertFile != "" && opts.Fingerprint != "":
		// Silently preferring one would apply a different trust policy
		// than the caller asked for.
		return nil, errors.New("--ca-cert and --fingerprint are mutually exclusive; supply exactly one")

	case opts.CACertFile != "":
		caPEM, err := os.ReadFile(opts.CACertFile)
		if err != nil {
			return nil, fmt.Errorf("--ca-cert: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("--ca-cert %s contains no certificates", opts.CACertFile)
		}
		return &tls.Config{
			MinVersion:       tls.VersionTLS13,
			CurvePreferences: hybridCurvePreferences(),
			RootCAs:          pool,
			ServerName:       name,
		}, nil

	case opts.Fingerprint != "":
		want, err := parseFingerprint(opts.Fingerprint)
		if err != nil {
			return nil, err
		}
		// Custom verification: find the pinned CA in the presented chain,
		// then verify the leaf against it including hostname. Standard
		// verification is disabled (InsecureSkipVerify) because the pin IS
		// the trust anchor; this callback replaces it, never skips it.
		return &tls.Config{
			MinVersion:         tls.VersionTLS13,
			CurvePreferences:   hybridCurvePreferences(),
			ServerName:         name,
			InsecureSkipVerify: true,
			VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
				return verifyByFingerprint(rawCerts, want, name)
			},
		}, nil

	default:
		return nil, errors.New("one of --ca-cert or --fingerprint is required (printed by `polarbeam-server token create`)")
	}
}

func verifyByFingerprint(rawCerts [][]byte, want [32]byte, hostname string) error {
	pool := x509.NewCertPool()
	var pinned bool
	certs := make([]*x509.Certificate, 0, len(rawCerts))
	for _, raw := range rawCerts {
		c, err := x509.ParseCertificate(raw)
		if err != nil {
			return fmt.Errorf("parse presented certificate: %w", err)
		}
		certs = append(certs, c)
		if sha256.Sum256(c.Raw) == want {
			pinned = true
			pool.AddCert(c)
		}
	}
	if !pinned {
		return errors.New("server did not present a certificate matching the pinned fingerprint")
	}
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	_, err := certs[0].Verify(x509.VerifyOptions{
		Roots:         pool,
		Intermediates: inter,
		DNSName:       hostname,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	if err != nil {
		return fmt.Errorf("server certificate does not chain to the pinned CA: %w", err)
	}
	return nil
}

func parseFingerprint(s string) ([32]byte, error) {
	var out [32]byte
	s = strings.TrimPrefix(s, "sha256:")
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != 32 {
		return out, fmt.Errorf("--fingerprint must be sha256:<64 hex chars>")
	}
	copy(out[:], raw)
	return out, nil
}

func serverName(cfg config.Config) string {
	if cfg.Server.SNI != "" {
		return cfg.Server.SNI
	}
	host, _, err := net.SplitHostPort(cfg.Server.Address)
	if err != nil {
		return cfg.Server.Address
	}
	return host
}
