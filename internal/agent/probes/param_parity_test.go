package probes

// probeadmin validates probe params on the server; the probers re-validate
// at run time to guard against version skew. Both sides carry "keep in
// lockstep" comments — these tests are the lockstep. probeadmin is imported
// by tests only, so the agent binary stays free of server code.

import (
	"testing"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
	"github.com/devalexllc/polarbeam/internal/server/probeadmin"
)

func adminParam(t *testing.T, typ pb.ProbeType, key string) probeadmin.ParamSpec {
	t.Helper()
	for _, spec := range probeadmin.Params(typ) {
		if spec.Key == key {
			return spec
		}
	}
	t.Fatalf("probeadmin declares no %q param for %v", key, typ)
	return probeadmin.ParamSpec{}
}

// assertEnumMatchesMap requires the server enum and the prober's map to be
// the same true set: no duplicates hiding a dropped entry behind a matching
// length, nothing accepted server-side the prober errors on, nothing the
// prober supports that operators cannot configure.
func assertEnumMatchesMap[V any](t *testing.T, key string, enum []string, prober map[string]V) {
	t.Helper()
	seen := map[string]bool{}
	for _, name := range enum {
		if seen[name] {
			t.Errorf("%s: probeadmin enum lists %s twice", key, name)
		}
		seen[name] = true
		if _, ok := prober[name]; !ok {
			t.Errorf("%s: probeadmin accepts %s but the prober would ERROR on it", key, name)
		}
	}
	for name := range prober {
		if !seen[name] {
			t.Errorf("%s: prober supports %s but probeadmin never offers it", key, name)
		}
	}
}

func TestDNSEnumsMatchProbeadmin(t *testing.T) {
	assertEnumMatchesMap(t, "dns.qtype",
		adminParam(t, pb.ProbeType_PROBE_TYPE_DNS, "dns.qtype").Enum, dnsQTypes)
	assertEnumMatchesMap(t, "dns.expect_rcode",
		adminParam(t, pb.ProbeType_PROBE_TYPE_DNS, "dns.expect_rcode").Enum, dnsRcodes)
}

func TestMTUBoundsMatchProbeadmin(t *testing.T) {
	if probeadmin.MTUFloor != mtuFloor || probeadmin.MTUCeil != mtuCeil {
		t.Errorf("bounds: probeadmin [%d, %d] vs prober [%d, %d]",
			probeadmin.MTUFloor, probeadmin.MTUCeil, mtuFloor, mtuCeil)
	}
	if probeadmin.DefaultMTUMin != defaultMTUMin || probeadmin.DefaultMTUMax != defaultMTUMax {
		t.Errorf("defaults: probeadmin [%d, %d] vs prober [%d, %d]",
			probeadmin.DefaultMTUMin, probeadmin.DefaultMTUMax, defaultMTUMin, defaultMTUMax)
	}
	for _, key := range []string{"mtu.min", "mtu.max"} {
		spec := adminParam(t, pb.ProbeType_PROBE_TYPE_PATH_MTU, key)
		if spec.Min != mtuFloor || spec.Max != mtuCeil {
			t.Errorf("%s: probeadmin range [%d, %d] vs prober [%d, %d]",
				key, spec.Min, spec.Max, mtuFloor, mtuCeil)
		}
	}
}

// TestHTTPStatusSyntaxMatchesProbeadmin: every http.expect_status value the
// server accepts must be one the prober can actually match against some
// real status code — an accepted-but-uninterpretable value would make a
// probe fail forever with no config error.
func TestHTTPStatusSyntaxMatchesProbeadmin(t *testing.T) {
	interpretable := func(expect string) bool {
		for code := 100; code <= 599; code++ {
			if statusMatches(expect, code) {
				return true
			}
		}
		return false
	}
	for _, v := range []string{"200", "204", "301", "404", "500", "599", "1xx", "2xx", "3xx", "4xx", "5xx"} {
		problems := probeadmin.ValidateParams(pb.ProbeType_PROBE_TYPE_HTTP, false,
			map[string]string{"http.expect_status": v})
		if len(problems) > 0 {
			t.Errorf("probeadmin rejects %q which this test assumed valid: %v", v, problems)
			continue
		}
		if !interpretable(v) {
			t.Errorf("probeadmin accepts http.expect_status=%q but statusMatches can never match it", v)
		}
	}
	for _, v := range []string{"", "20x", "x00", "600", "0xx", "6xx", "abc", "2XX"} {
		if problems := probeadmin.ValidateParams(pb.ProbeType_PROBE_TYPE_HTTP, false,
			map[string]string{"http.expect_status": v}); len(problems) == 0 {
			t.Errorf("probeadmin accepts %q — extend the prober parity if that syntax is now legal", v)
		}
	}
}
