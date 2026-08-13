package probeadmin

import (
	"strings"
	"testing"
	"time"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
)

func TestParseType(t *testing.T) {
	for name, want := range TypeNames {
		got, err := ParseType(name)
		if err != nil || got != want {
			t.Errorf("ParseType(%q) = %v, %v; want %v", name, got, err, want)
		}
	}
	_, err := ParseType("smtp")
	if err == nil {
		t.Fatal("ParseType(smtp) succeeded")
	}
	// The accepted list must be sorted so error text is deterministic.
	want := `unknown probe type "smtp" (accepted: dns, http, icmp, ntp, tcp, tls, traceroute)`
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

func TestTypeName(t *testing.T) {
	if got := TypeName(int16(pb.ProbeType_PROBE_TYPE_DNS)); got != "dns" {
		t.Errorf("TypeName(dns) = %q", got)
	}
	if got := TypeName(99); got != "type-99" {
		t.Errorf("TypeName(99) = %q", got)
	}
}

var cliFields = FieldNames{
	Interval: "--interval", Timeout: "--timeout",
	TrainCount: "--train-count", TrainSpacing: "--train-spacing",
}

func TestValidateSettings(t *testing.T) {
	cases := []struct {
		name              string
		typ               pb.ProbeType
		interval, timeout time.Duration
		count             int
		spacing           time.Duration
		wantContains      []string
	}{
		{"ok", pb.ProbeType_PROBE_TYPE_TCP, 30 * time.Second, 5 * time.Second, 0, 0, nil},
		{"ok train", pb.ProbeType_PROBE_TYPE_ICMP, 30 * time.Second, 5 * time.Second, 10, 200 * time.Millisecond, nil},
		{"non-positive", pb.ProbeType_PROBE_TYPE_TCP, 0, 0, 0, 0, []string{"--interval and --timeout must be positive"}},
		{"timeout too long", pb.ProbeType_PROBE_TYPE_TCP, 10 * time.Second, 10 * time.Second, 0, 0,
			[]string{"--timeout (10s) must be shorter than --interval (10s)"}},
		{"negative train", pb.ProbeType_PROBE_TYPE_ICMP, 30 * time.Second, 5 * time.Second, -1, 0,
			[]string{"--train-count and --train-spacing must not be negative"}},
		{"spacing without count", pb.ProbeType_PROBE_TYPE_ICMP, 30 * time.Second, 5 * time.Second, 0, time.Second,
			[]string{"--train-spacing requires --train-count"}},
		{"train too long", pb.ProbeType_PROBE_TYPE_ICMP, 30 * time.Second, 5 * time.Second, 30, 200 * time.Millisecond,
			[]string{"train of 30 × 200ms (6s) must fit inside --timeout (5s)"}},
		{"train default spacing", pb.ProbeType_PROBE_TYPE_ICMP, 30 * time.Second, time.Second, 10, 0,
			[]string{"train of 10 × 200ms (2s) must fit inside --timeout (1s)"}},
		{"multiple problems", pb.ProbeType_PROBE_TYPE_ICMP, 5 * time.Second, 10 * time.Second, -1, -1,
			[]string{"must be shorter than", "must not be negative"}},
		// The implicit train: a zero count on a train type still runs the
		// prober defaults (10 × 200ms), so the budget check must fire —
		// this exact shape used to pass validation and then time out every
		// run (issue #34).
		{"icmp implicit train too long", pb.ProbeType_PROBE_TYPE_ICMP, 30 * time.Second, time.Second, 0, 0,
			[]string{"icmp's default train of 10 × 200ms (2s) must fit inside --timeout (1s); raise --timeout or set --train-count/--train-spacing"}},
		{"icmp implicit train boundary", pb.ProbeType_PROBE_TYPE_ICMP, 30 * time.Second, 2 * time.Second, 0, 0,
			[]string{"icmp's default train of 10 × 200ms (2s) must fit inside --timeout (2s)"}},
		{"icmp implicit train fits", pb.ProbeType_PROBE_TYPE_ICMP, 30 * time.Second, 5 * time.Second, 0, 0, nil},
		{"non-train type short timeout ok", pb.ProbeType_PROBE_TYPE_TCP, 30 * time.Second, time.Second, 0, 0, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ValidateSettings(c.typ, c.interval, c.timeout, c.count, c.spacing, cliFields)
			if len(c.wantContains) == 0 {
				if len(got) != 0 {
					t.Fatalf("problems = %v, want none", got)
				}
				return
			}
			joined := strings.Join(got, "; ")
			for _, w := range c.wantContains {
				if !strings.Contains(joined, w) {
					t.Errorf("problems %q missing %q", joined, w)
				}
			}
			if len(got) != len(c.wantContains) {
				t.Errorf("got %d problems %v, want %d", len(got), got, len(c.wantContains))
			}
		})
	}
}

func TestValidateParams(t *testing.T) {
	cases := []struct {
		name         string
		typ          pb.ProbeType
		mesh         bool
		params       map[string]string
		wantContains []string
	}{
		{"icmp none ok", pb.ProbeType_PROBE_TYPE_ICMP, true, nil, nil},
		{"icmp unknown key", pb.ProbeType_PROBE_TYPE_ICMP, true, map[string]string{"port": "9"},
			[]string{`unknown key "port" for probe type icmp (accepted: none)`}},
		{"tcp mesh needs port", pb.ProbeType_PROBE_TYPE_TCP, true, nil,
			[]string{`"port" is required for mesh tcp probes`}},
		{"tcp mesh port ok", pb.ProbeType_PROBE_TYPE_TCP, true, map[string]string{"port": "5432"}, nil},
		{"tcp direct rejects port", pb.ProbeType_PROBE_TYPE_TCP, false, map[string]string{"port": "5432"},
			[]string{`"port" applies only to mesh probes`}},
		{"tcp direct ok", pb.ProbeType_PROBE_TYPE_TCP, false, nil, nil},
		{"port range", pb.ProbeType_PROBE_TYPE_TCP, true, map[string]string{"port": "70000"},
			[]string{`"port" must be an integer between 1 and 65535, got "70000"`}},
		{"port not int", pb.ProbeType_PROBE_TYPE_TCP, true, map[string]string{"port": "https"},
			[]string{`"port" must be an integer between 1 and 65535`}},
		{"tls bool bad", pb.ProbeType_PROBE_TYPE_TLS, true,
			map[string]string{"port": "443", "tls.insecure_skip_verify": "yes"},
			[]string{`"tls.insecure_skip_verify" must be "true" or "false", got "yes"`}},
		{"tls ok", pb.ProbeType_PROBE_TYPE_TLS, true,
			map[string]string{"port": "443", "tls.sni": "example.org", "tls.insecure_skip_verify": "true"}, nil},
		{"http status class ok", pb.ProbeType_PROBE_TYPE_HTTP, false,
			map[string]string{"http.expect_status": "4xx"}, nil},
		{"http status exact ok", pb.ProbeType_PROBE_TYPE_HTTP, false,
			map[string]string{"http.expect_status": "204"}, nil},
		{"http status bad", pb.ProbeType_PROBE_TYPE_HTTP, false,
			map[string]string{"http.expect_status": "6xx"},
			[]string{`"http.expect_status" must be an exact status ("200") or a class ("2xx"), got "6xx"`}},
		{"http method spaces", pb.ProbeType_PROBE_TYPE_HTTP, false,
			map[string]string{"http.method": "GET /"},
			[]string{`"http.method" must be a single HTTP method token`}},
		{"dns requires qname both modes", pb.ProbeType_PROBE_TYPE_DNS, false, nil,
			[]string{`"dns.qname" is required for dns probes`}},
		{"dns qtype case-insensitive", pb.ProbeType_PROBE_TYPE_DNS, true,
			map[string]string{"dns.qname": "example.org", "dns.qtype": "aaaa"}, nil},
		{"dns qtype bad", pb.ProbeType_PROBE_TYPE_DNS, true,
			map[string]string{"dns.qname": "example.org", "dns.qtype": "ANY"},
			[]string{`unsupported dns.qtype "ANY" (accepted: A, AAAA, CNAME, MX, NS, PTR, SOA, SRV, TXT)`}},
		{"dns rcode bad", pb.ProbeType_PROBE_TYPE_DNS, true,
			map[string]string{"dns.qname": "example.org", "dns.expect_rcode": "YES"},
			[]string{`unsupported dns.expect_rcode "YES"`}},
		{"empty string value", pb.ProbeType_PROBE_TYPE_DNS, true,
			map[string]string{"dns.qname": "  "},
			[]string{`"dns.qname" must not be empty`}},
		{"multiple problems", pb.ProbeType_PROBE_TYPE_TLS, true,
			map[string]string{"bogus": "1", "tls.insecure_skip_verify": "nah"},
			[]string{`unknown key "bogus"`, `must be "true" or "false"`, `"port" is required`}},
		{"http mesh rejected", pb.ProbeType_PROBE_TYPE_HTTP, true, nil,
			[]string{"http probes cannot be mesh templates"}},
		{"http direct ok", pb.ProbeType_PROBE_TYPE_HTTP, false, nil, nil},
		{"ntp mesh rejected", pb.ProbeType_PROBE_TYPE_NTP, true, nil,
			[]string{"ntp probes cannot be mesh templates"}},
		{"ntp unknown key", pb.ProbeType_PROBE_TYPE_NTP, false, map[string]string{"ntp.version": "4"},
			[]string{`unknown key "ntp.version" for probe type ntp (accepted: none)`}},
		{"ntp direct ok", pb.ProbeType_PROBE_TYPE_NTP, false, nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ValidateParams(c.typ, c.mesh, c.params)
			joined := strings.Join(got, "; ")
			if len(c.wantContains) == 0 {
				if len(got) != 0 {
					t.Fatalf("problems = %v, want none", got)
				}
				return
			}
			for _, w := range c.wantContains {
				if !strings.Contains(joined, w) {
					t.Errorf("problems %q missing %q", joined, w)
				}
			}
			if len(got) != len(c.wantContains) {
				t.Errorf("got %d problems %v, want %d", len(got), got, len(c.wantContains))
			}
		})
	}
}

// TestRegistryCoversAllTypes pins that every admin-facing type has a
// registry entry, so a future probe type cannot silently bypass param
// validation.
func TestRegistryCoversAllTypes(t *testing.T) {
	for name, typ := range TypeNames {
		if _, ok := registry[typ]; !ok {
			t.Errorf("probe type %s has no registry entry", name)
		}
	}
}

// TestValidateMeshMembers pins the rule that silently cost an operator a
// configured-but-dead mesh: expansion needs two distinct sites, so anything
// smaller must be refused at write time rather than accepted and ignored.
func TestValidateMeshMembers(t *testing.T) {
	for _, c := range []struct {
		name    string
		members int
		wantErr bool
	}{
		{"empty mesh", 0, true},
		{"single site", 1, true},
		{"minimum viable", 2, false},
		{"larger mesh", 5, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := ValidateMeshMembers("edge", c.members)
			if c.wantErr != (len(got) > 0) {
				t.Fatalf("ValidateMeshMembers(%d) = %v, wantErr %v", c.members, got, c.wantErr)
			}
			if !c.wantErr {
				return
			}
			// The message must name the mesh and the requirement: an
			// operator reads this instead of watching an empty dashboard.
			joined := strings.Join(got, "; ")
			for _, want := range []string{`"edge"`, "distinct sites"} {
				if !strings.Contains(joined, want) {
					t.Errorf("message %q missing %q", joined, want)
				}
			}
		})
	}
}

// TestWarnings pins the advisories. They must stay warnings: agents that
// genuinely run resolvers make the mesh-dns configuration correct, and an
// operator probing their own time server may poll faster than public NTP
// etiquette allows, so neither can become a hard error.
func TestWarnings(t *testing.T) {
	dnsQ := map[string]string{"dns.qname": "example.internal"}
	dnsResolved := map[string]string{"dns.qname": "example.internal", "dns.resolver": "10.0.0.53:53"}

	for _, c := range []struct {
		name     string
		typ      pb.ProbeType
		mesh     bool
		interval time.Duration
		params   map[string]string
		wantSub  string // "" = no warning expected
	}{
		{"mesh dns without resolver warns", pb.ProbeType_PROBE_TYPE_DNS, true, 30 * time.Second, dnsQ, "dns.resolver"},
		{"mesh dns with explicit resolver is silent", pb.ProbeType_PROBE_TYPE_DNS, true, 30 * time.Second, dnsResolved, ""},
		{"direct dns is silent", pb.ProbeType_PROBE_TYPE_DNS, false, 30 * time.Second, dnsQ, ""},
		{"mesh icmp is silent", pb.ProbeType_PROBE_TYPE_ICMP, true, 30 * time.Second, nil, ""},
		{"ntp under a minute warns", pb.ProbeType_PROBE_TYPE_NTP, false, 30 * time.Second, nil, "rate limiting"},
		{"ntp at a minute is silent", pb.ProbeType_PROBE_TYPE_NTP, false, 60 * time.Second, nil, ""},
		{"ntp above a minute is silent", pb.ProbeType_PROBE_TYPE_NTP, false, 5 * time.Minute, nil, ""},
		{"fast interval on other types is silent", pb.ProbeType_PROBE_TYPE_TCP, false, time.Second, nil, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := Warnings(c.typ, c.mesh, c.interval, c.params)
			if (c.wantSub != "") != (len(got) > 0) {
				t.Fatalf("Warnings = %v, want warning %v", got, c.wantSub != "")
			}
			if c.wantSub != "" && !strings.Contains(strings.Join(got, "; "), c.wantSub) {
				t.Errorf("warning %v must mention %q", got, c.wantSub)
			}
		})
	}
}

// TestWarningsNeverBlock pins that warnings are advisory only: any config
// that warns must still pass validation, or the warning would be a lie.
func TestWarningsNeverBlock(t *testing.T) {
	params := map[string]string{"dns.qname": "example.internal"}
	if len(Warnings(pb.ProbeType_PROBE_TYPE_DNS, true, 30*time.Second, params)) == 0 {
		t.Fatal("expected the mesh-dns warning for this fixture")
	}
	if problems := ValidateParams(pb.ProbeType_PROBE_TYPE_DNS, true, params); len(problems) > 0 {
		t.Errorf("a warned config must still validate, got %v", problems)
	}

	if len(Warnings(pb.ProbeType_PROBE_TYPE_NTP, false, 30*time.Second, nil)) == 0 {
		t.Fatal("expected the ntp cadence warning for this fixture")
	}
	if problems := ValidateSettings(pb.ProbeType_PROBE_TYPE_NTP, 30*time.Second, 5*time.Second, 0, 0, cliFields); len(problems) > 0 {
		t.Errorf("a warned config must still validate, got %v", problems)
	}
}

// TestValidateMeshMemberRemoval pins the second route to a dead mesh probe.
// Creation refuses a too-small mesh; removal must refuse to shrink a probed
// mesh below the same floor, or the invariant is trivially bypassed.
func TestValidateMeshMemberRemoval(t *testing.T) {
	for _, c := range []struct {
		name         string
		membersAfter int
		templates    int
		wantRefusal  bool
	}{
		{"probed mesh dropping to one member", 1, 2, true},
		{"probed mesh emptied", 0, 1, true},
		{"probed mesh staying viable", 2, 3, false},
		{"unprobed mesh may shrink freely", 1, 0, false},
		{"unprobed mesh may empty", 0, 0, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := ValidateMeshMemberRemoval("edge", c.membersAfter, c.templates)
			if c.wantRefusal != (len(got) > 0) {
				t.Fatalf("ValidateMeshMemberRemoval(%d, %d) = %v, want refusal %v",
					c.membersAfter, c.templates, got, c.wantRefusal)
			}
			if !c.wantRefusal {
				return
			}
			// The operator needs the way out, not just the refusal.
			joined := strings.Join(got, "; ")
			for _, want := range []string{`"edge"`, "delete the probe"} {
				if !strings.Contains(joined, want) {
					t.Errorf("message %q missing %q", joined, want)
				}
			}
		})
	}
}
