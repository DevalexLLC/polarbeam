package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const tlsUsage = "usage: polarbeam-server tls install --config <file> --cert <pem> --key <pem> [--owner 10001]"

// renameFile is os.Rename behind a seam so tests can fail the second of
// the two live-path renames in installTLS.
var renameFile = os.Rename

// cmdTLS installs the operator-managed dashboard certificate and key into
// the paths named by tls.cert_file / tls.key_file. It exists because the
// release image has no shell: the former runbook was `cp` + `chown` +
// `chmod` through `--entrypoint sh`, and this replaces it with something
// that also refuses a mismatched pair instead of leaving it for `serve`
// to trip over at the next restart.
func cmdTLS(args []string) error {
	if len(args) < 1 || args[0] != "install" {
		return errors.New(tlsUsage)
	}
	fs := flag.NewFlagSet("tls install", flag.ExitOnError)
	certIn := fs.String("cert", "", "PEM certificate to install (leaf first, then any intermediates)")
	keyIn := fs.String("key", "", "PEM private key matching --cert")
	owner := fs.Int("owner", 10001, "uid the installed files are handed to when running as root (the image's server uid)")
	cfg, err := loadConfig(fs, args[1:])
	if err != nil {
		return err
	}
	if problems := tlsInstallProblems(*certIn, *keyIn, *owner); len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	summary, err := installTLS(*certIn, *keyIn, cfg.TLS.CertFile, cfg.TLS.KeyFile, *owner, os.Geteuid() == 0)
	if err != nil {
		return err
	}
	fmt.Print(summary)
	return nil
}

// tlsInstallProblems returns every flag problem at once (fail-loud: name
// all problems, not just the first).
func tlsInstallProblems(certIn, keyIn string, owner int) []string {
	var problems []string
	if certIn == "" {
		problems = append(problems, "--cert is required")
	}
	if keyIn == "" {
		problems = append(problems, "--key is required")
	}
	if owner < 0 {
		problems = append(problems, "--owner must be a uid (non-negative)")
	}
	return problems
}

// installTLS validates the certificate/key pair and replaces the configured
// files with it. It returns a human-readable summary of what was installed.
//
// Guarantee, exactly as documented for operators: everything that can fail
// for ordinary reasons — reading and validating the inputs, creating the
// destination directory, writing both staged files, setting their modes
// and (as root) ownership — happens before either live path is touched,
// and any failure there removes the staged files and leaves the existing
// pair intact. What remains is two same-directory renames, key first. If
// the second one fails, the error names both live paths and the staged
// certificate left behind and says the live pair is mismatched until the
// command is rerun; no silent restore is attempted.
func installTLS(certIn, keyIn, certDst, keyDst string, owner int, asRoot bool) (string, error) {
	certPEM, err := os.ReadFile(certIn)
	if err != nil {
		return "", fmt.Errorf("read certificate: %w", err)
	}
	keyPEM, err := os.ReadFile(keyIn)
	if err != nil {
		return "", fmt.Errorf("read key: %w", err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return "", fmt.Errorf("%s and %s do not form a valid certificate/key pair: %w", certIn, keyIn, err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return "", fmt.Errorf("parse leaf certificate: %w", err)
	}
	now := time.Now()
	if now.After(leaf.NotAfter) {
		return "", fmt.Errorf("refusing to install %s: certificate expired %s", certIn, leaf.NotAfter.UTC().Format(time.RFC3339))
	}
	if filepath.Clean(certDst) == filepath.Clean(keyDst) {
		return "", fmt.Errorf("tls.cert_file and tls.key_file are the same path (%s)", certDst)
	}
	for _, dst := range []string{certDst, keyDst} {
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", fmt.Errorf("create %s: %w", filepath.Dir(dst), err)
		}
	}

	// Phase 1: stage both files completely. Nothing live is touched yet.
	stagedCert, err := stageFile(certDst, certPEM, 0o644, owner, asRoot)
	if err != nil {
		return "", fmt.Errorf("stage certificate: %w (existing pair untouched)", err)
	}
	stagedKey, err := stageFile(keyDst, keyPEM, 0o600, owner, asRoot)
	if err != nil {
		os.Remove(stagedCert)
		return "", fmt.Errorf("stage key: %w (existing pair untouched)", err)
	}

	// Phase 2: two same-directory renames, key first.
	if err := renameFile(stagedKey, keyDst); err != nil {
		os.Remove(stagedCert)
		os.Remove(stagedKey)
		return "", fmt.Errorf("install %s: %w (existing pair untouched)", keyDst, err)
	}
	if err := renameFile(stagedCert, certDst); err != nil {
		return "", fmt.Errorf("installed %s but could not install %s: %w — the live pair is MISMATCHED until this command is rerun; the staged certificate was left at %s", keyDst, certDst, err, stagedCert)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "installed %s (0644) and %s (0600)", certDst, keyDst)
	if asRoot {
		fmt.Fprintf(&b, ", owner uid %d", owner)
	}
	fmt.Fprintf(&b, "\n  subject: %s\n", leaf.Subject)
	if sans := certSANs(leaf); len(sans) > 0 {
		fmt.Fprintf(&b, "  SANs:    %s\n", strings.Join(sans, ", "))
	}
	fmt.Fprintf(&b, "  expires: %s (in %d days)\n", leaf.NotAfter.UTC().Format(time.RFC3339), int(leaf.NotAfter.Sub(now).Hours()/24))
	fmt.Fprintf(&b, "  chain:   %d certificate(s)\n", len(pair.Certificate))
	fmt.Fprint(&b, "restart the server to load the new certificate; it is read only at startup\n")
	return b.String(), nil
}

// stageFile writes data to a temp file beside dst with the final mode and
// (as root) owner already applied, so the later rename is the only step
// left. On any failure the temp file is removed and the error returned.
func stageFile(dst string, data []byte, mode os.FileMode, owner int, asRoot bool) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".*.tmp")
	if err != nil {
		return "", err
	}
	path := f.Name()
	fail := func(err error) (string, error) {
		f.Close()
		os.Remove(path)
		return "", fmt.Errorf("%s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		return fail(err)
	}
	if err := f.Chmod(mode); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("%s: %w", path, err)
	}
	if asRoot {
		// uid only: group is left as created, the key is 0600 and the
		// certificate world-readable, so the gid carries no permission.
		if err := os.Chown(path, owner, -1); err != nil {
			os.Remove(path)
			return "", fmt.Errorf("%s: %w", path, err)
		}
	}
	return path, nil
}

func certSANs(c *x509.Certificate) []string {
	var sans []string
	sans = append(sans, c.DNSNames...)
	for _, ip := range c.IPAddresses {
		sans = append(sans, ip.String())
	}
	for _, u := range c.URIs {
		sans = append(sans, u.String())
	}
	sans = append(sans, c.EmailAddresses...)
	return sans
}
