package probes

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/net/icmp"

	"github.com/devalexllc/polarbeam/internal/agent/enroll"
)

// Check is one selfcheck result. Fatal checks must pass for the agent to be
// able to do its job; non-fatal ones are informational (e.g. IPv6 probing on
// a v4-only host).
type Check struct {
	Name   string
	OK     bool
	Detail string
	Fatal  bool
}

// SelfCheck probes the capabilities the M4 probers need so problems surface
// at install time, not as ERROR results at 2 AM. The ICMP prober works with
// EITHER a datagram or a raw socket; traceroute strictly needs raw.
func SelfCheck(stateDir string) []Check {
	var checks []Check

	dgram := trySocket("udp4")
	raw := trySocket("ip4:icmp")
	checks = append(checks,
		Check{
			Name: "icmp (datagram)", OK: dgram == nil, Fatal: false,
			Detail: detailOr(dgram, "unprivileged datagram ICMP available",
				"check net.ipv4.ping_group_range covers the service group"),
		},
		Check{
			Name: "icmp (raw)", OK: raw == nil, Fatal: false,
			Detail: detailOr(raw, "CAP_NET_RAW available",
				"grant CAP_NET_RAW (container: --cap-add NET_RAW) for the raw fallback"),
		},
		// The echo prober needs at least one of the two socket modes.
		Check{
			Name: "icmp", OK: dgram == nil || raw == nil, Fatal: true,
			Detail: pick(dgram == nil || raw == nil,
				"echo probing available",
				"no ICMP socket mode works: set ping_group_range or grant CAP_NET_RAW"),
		},
		Check{
			Name: "traceroute", OK: raw == nil, Fatal: false,
			Detail: detailOr(raw, "raw ICMP socket available",
				"traceroute requires a raw ICMP socket (CAP_NET_RAW)"),
		},
	)

	if v6 := trySocket("udp6"); v6 == nil {
		checks = append(checks, Check{Name: "icmp6 (datagram)", OK: true, Detail: "unprivileged datagram ICMPv6 available"})
	} else if v6raw := trySocket("ip6:ipv6-icmp"); v6raw == nil {
		checks = append(checks, Check{Name: "icmp6 (raw)", OK: true, Detail: "raw ICMPv6 available"})
	} else {
		checks = append(checks, Check{
			Name: "icmp6", OK: false, Fatal: false,
			Detail: "no ICMPv6 socket mode works; v6 targets will fail (fine on v4-only hosts)",
		})
	}

	checks = append(checks, identityChecks(stateDir, time.Now())...)
	checks = append(checks, spoolCheck(stateDir))
	return checks
}

// identityChecks reports PKI state. Not being enrolled is OK-informational:
// the container entrypoint runs selfcheck before `run`, and a fresh
// deployment must be able to reach its first `enroll` without the
// preflight refusing to explain itself.
func identityChecks(stateDir string, now time.Time) []Check {
	pki := enroll.NewPKI(stateDir)
	if !pki.Enrolled() {
		return []Check{{
			Name: "identity", OK: true,
			Detail: "not enrolled yet — run `polarbeam-agent enroll` with a join token",
		}}
	}

	var checks []Check
	leaf, err := pki.Leaf()
	switch {
	case err != nil:
		checks = append(checks, Check{
			Name: "identity", Fatal: true,
			Detail: fmt.Sprintf("%v — re-enroll with a fresh token", err),
		})
	case now.After(leaf.NotAfter):
		checks = append(checks, Check{
			Name: "identity", Fatal: true,
			Detail: fmt.Sprintf("certificate expired %s — the server rejects expired certificates; re-enroll with a fresh token",
				leaf.NotAfter.Format(time.RFC3339)),
		})
	case leaf.NotAfter.Sub(now) < leaf.NotAfter.Sub(leaf.NotBefore)/3:
		// The renewer fires at 2/3 of validity; being inside the final
		// third means renewal has been failing since then.
		checks = append(checks, Check{
			Name: "identity", OK: false, Fatal: false,
			Detail: fmt.Sprintf("certificate expires %s and renewal appears to be failing — check connectivity to the control plane",
				leaf.NotAfter.Format(time.RFC3339)),
		})
	default:
		checks = append(checks, Check{
			Name: "identity", OK: true,
			Detail: fmt.Sprintf("certificate valid until %s (%dd remaining)",
				leaf.NotAfter.Format(time.RFC3339), int(leaf.NotAfter.Sub(now).Hours()/24)),
		})
	}

	// A group- or world-readable private key is a leaked identity: any
	// local user could impersonate this agent. Fail loudly rather than
	// carry on with a compromised credential.
	perm := Check{Name: "pki permissions", Fatal: true}
	fi, err := os.Stat(pki.KeyPath())
	switch {
	case err != nil:
		perm.Detail = fmt.Sprintf("cannot stat %s: %v — re-enroll with a fresh token", pki.KeyPath(), err)
	case fi.Mode().Perm()&0o077 != 0:
		perm.Detail = fmt.Sprintf("%s is mode %o — must be readable only by the service user (chmod 600)",
			pki.KeyPath(), fi.Mode().Perm())
	default:
		perm.OK = true
		perm.Detail = fmt.Sprintf("%s is mode %o", pki.KeyPath(), fi.Mode().Perm())
	}
	return append(checks, perm)
}

func trySocket(network string) error {
	conn, err := icmp.ListenPacket(network, "")
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

func detailOr(err error, ok, remedy string) string {
	if err == nil {
		return ok
	}
	return fmt.Sprintf("%v — %s", err, remedy)
}

func pick(ok bool, yes, no string) string {
	if ok {
		return yes
	}
	return no
}

// spoolCheck verifies the spool directory is creatable and writable by
// creating and removing a probe file.
func spoolCheck(stateDir string) Check {
	dir := filepath.Join(stateDir, "spool")
	c := Check{Name: "spool", Fatal: true}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		c.Detail = fmt.Sprintf("cannot create %s: %v", dir, err)
		return c
	}
	probe := filepath.Join(dir, ".selfcheck")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		c.Detail = fmt.Sprintf("cannot write in %s: %v", dir, err)
		return c
	}
	os.Remove(probe)
	c.OK = true
	c.Detail = dir + " writable"
	return c
}
