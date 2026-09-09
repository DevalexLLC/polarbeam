package probes

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	pmtu := tryPathMTU()
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
		Check{
			Name: "path_mtu", OK: pmtu == nil, Fatal: false,
			Detail: detailOr(pmtu, "raw ICMP socket and PMTU-probe socket options available",
				"path MTU probing requires a raw ICMP socket (CAP_NET_RAW) and Linux PMTU-probe socket options"),
		},
	)
	// The socket rows say WHETHER raw ICMP works; this one says why not
	// when it does not (capability granted but not effective, or absent).
	if c, ok := capabilityCheck(); ok {
		checks = append(checks, c)
	}

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
// `run` performs this selfcheck as its preflight, and a fresh deployment
// must be able to reach its first `enroll` without the preflight refusing
// to explain itself (`run` itself then refuses in uplink.New).
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

// tryPathMTU verifies both requirements of the path MTU prober: a raw
// ICMP socket and the Linux PMTU-probe socket options.
func tryPathMTU() error {
	conn, err := net.ListenPacket("ip4:icmp", "")
	if err != nil {
		return err
	}
	defer conn.Close()
	ipConn, ok := conn.(*net.IPConn)
	if !ok {
		return fmt.Errorf("unexpected raw socket type %T", conn)
	}
	return setDontFragment(ipConn, false)
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
// creating and removing a probe file, then reports what is on disk. The
// usage figure replaces `du -sh` on the spool, which the shell-less image
// cannot run; it is informational and never fails the check.
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
	size, segments, err := spoolUsage(dir)
	if err != nil {
		c.Detail = fmt.Sprintf("%s writable; usage unknown: %v", dir, err)
		return c
	}
	c.Detail = fmt.Sprintf("%s writable; %s on disk in %d segments", dir, fmtBytes(size), segments)
	return c
}

// spoolUsage sums the regular files directly under dir (the spool is flat)
// and counts *.seg segments. Read-only and safe beside a running agent: a
// segment acked and deleted between ReadDir and Info is simply skipped.
func spoolUsage(dir string) (size int64, segments int, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0, err
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return 0, 0, err
		}
		size += fi.Size()
		if strings.HasSuffix(e.Name(), ".seg") {
			segments++
		}
	}
	return size, segments, nil
}

// fmtBytes renders a byte count in 1024-based units, one decimal from MiB up.
func fmtBytes(n int64) string {
	const (
		kib = 1 << 10
		mib = 1 << 20
		gib = 1 << 30
	)
	switch {
	case n >= gib:
		return fmt.Sprintf("%.1f GiB", float64(n)/gib)
	case n >= mib:
		return fmt.Sprintf("%.1f MiB", float64(n)/mib)
	case n >= kib:
		return fmt.Sprintf("%d KiB", n/kib)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// capNetRaw is CAP_NET_RAW's bit index (<linux/capability.h>).
const capNetRaw = 13

// capabilityCheck reads the bounding and effective capability sets from
// /proc/self/status and reports where NET_RAW stands. It explains a raw
// ICMP failure that the socket rows only observe: the capability can be in
// the bounding set yet not effective when the binary's file capability was
// not applied (user-namespace remap, no-new-privileges, a nosuid mount).
// The row is omitted, not failed, where /proc is unavailable. Note that a
// container whose bounding set lacks NET_RAW normally never gets this far:
// the kernel refuses to exec a binary whose effective file capability it
// cannot grant.
func capabilityCheck() (Check, bool) {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return Check{}, false
	}
	defer f.Close()
	bnd, eff, err := parseCapStatus(f)
	if err != nil {
		return Check{}, false
	}
	c := Check{Name: "capabilities", Fatal: false}
	sets := fmt.Sprintf("CapBnd=%016x CapEff=%016x", bnd, eff)
	switch {
	case eff&(1<<capNetRaw) != 0:
		c.OK = true
		c.Detail = "NET_RAW effective (" + sets + ")"
	case bnd&(1<<capNetRaw) != 0:
		c.Detail = "NET_RAW is in the bounding set but not effective — the file capability was not applied (user-namespace remap, no-new-privileges, or a nosuid mount) (" + sets + ")"
	default:
		c.Detail = "NET_RAW is outside the bounding set — add --cap-add NET_RAW (" + sets + ")"
	}
	return c, true
}

// parseCapStatus extracts the CapBnd and CapEff hex masks from a
// /proc/<pid>/status body.
func parseCapStatus(r io.Reader) (bnd, eff uint64, err error) {
	var haveBnd, haveEff bool
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "CapBnd:"); ok {
			if bnd, err = strconv.ParseUint(strings.TrimSpace(v), 16, 64); err != nil {
				return 0, 0, fmt.Errorf("CapBnd: %w", err)
			}
			haveBnd = true
		} else if v, ok := strings.CutPrefix(line, "CapEff:"); ok {
			if eff, err = strconv.ParseUint(strings.TrimSpace(v), 16, 64); err != nil {
				return 0, 0, fmt.Errorf("CapEff: %w", err)
			}
			haveEff = true
		}
	}
	if err := sc.Err(); err != nil {
		return 0, 0, err
	}
	if !haveBnd || !haveEff {
		return 0, 0, errors.New("CapBnd/CapEff not found")
	}
	return bnd, eff, nil
}
