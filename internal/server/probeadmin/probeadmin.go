// Package probeadmin is the single source of truth for probe-config
// validation: admin-facing type names, cadence/train rules, and the
// per-type param registry. Both the admin CLI and the HTTP config API
// validate through this package so the two surfaces cannot drift.
package probeadmin

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
)

// TypeNames maps admin-facing names to wire enum values.
var TypeNames = map[string]pb.ProbeType{
	"icmp":       pb.ProbeType_PROBE_TYPE_ICMP,
	"tcp":        pb.ProbeType_PROBE_TYPE_TCP,
	"tls":        pb.ProbeType_PROBE_TYPE_TLS,
	"http":       pb.ProbeType_PROBE_TYPE_HTTP,
	"dns":        pb.ProbeType_PROBE_TYPE_DNS,
	"ntp":        pb.ProbeType_PROBE_TYPE_NTP,
	"traceroute": pb.ProbeType_PROBE_TYPE_TRACEROUTE,
}

// Names returns the accepted type names, sorted for deterministic output.
func Names() []string {
	names := make([]string, 0, len(TypeNames))
	for k := range TypeNames {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// ParseType resolves an admin-facing type name.
func ParseType(name string) (pb.ProbeType, error) {
	t, ok := TypeNames[name]
	if !ok {
		return 0, fmt.Errorf("unknown probe type %q (accepted: %s)", name, strings.Join(Names(), ", "))
	}
	return t, nil
}

// TypeName is the reverse mapping for display.
func TypeName(t int16) string {
	for name, v := range TypeNames {
		if int16(v) == t {
			return name
		}
	}
	return fmt.Sprintf("type-%d", t)
}

// Prober defaults applied when train fields are zero; the validation below
// must budget with the same values the agent will actually use.
const (
	DefaultTrainCount   = 10
	DefaultTrainSpacing = 200 * time.Millisecond
)

// FieldNames carries the surface-specific spelling of each setting so one
// validator produces natural error text for both the CLI ("--interval")
// and the HTTP API ("interval_ms").
type FieldNames struct {
	Interval     string
	Timeout      string
	TrainCount   string
	TrainSpacing string
}

// trainType reports types whose prober runs a packet train — ICMP is the
// only one (internal/agent/probes/icmp.go); other probers ignore the train
// fields entirely, so there is nothing to budget for them.
func trainType(t pb.ProbeType) bool { return t == pb.ProbeType_PROBE_TYPE_ICMP }

// ValidateSettings checks cadence and train rules, returning EVERY problem
// (settings.go convention: a request failing several ways gets the full
// list at once). The train must fit inside the per-run timeout because the
// agent budgets the whole train within it — a longer train would silently
// lose its tail. For train types that budget covers the IMPLICIT train too:
// the prober substitutes the defaults above for zero fields, so a zero-train
// icmp probe with a short timeout would otherwise time out every run.
func ValidateSettings(t pb.ProbeType, interval, timeout time.Duration, trainCount int, trainSpacing time.Duration, f FieldNames) []string {
	var problems []string
	if interval <= 0 || timeout <= 0 {
		problems = append(problems, fmt.Sprintf("%s and %s must be positive", f.Interval, f.Timeout))
	} else if timeout >= interval {
		problems = append(problems, fmt.Sprintf("%s (%s) must be shorter than %s (%s)", f.Timeout, timeout, f.Interval, interval))
	}
	switch {
	case trainCount < 0 || trainSpacing < 0:
		problems = append(problems, fmt.Sprintf("%s and %s must not be negative", f.TrainCount, f.TrainSpacing))
	case trainCount == 0 && trainSpacing > 0:
		// Snapshot building only forwards spacing alongside a positive
		// count; accepting spacing alone would silently no-op on the agent.
		problems = append(problems, fmt.Sprintf("%s requires %s", f.TrainSpacing, f.TrainCount))
	case timeout <= 0:
		// Cadence already failed above; there is no budget to check against.
	case trainCount > 0:
		effSpacing := trainSpacing
		if effSpacing == 0 {
			effSpacing = DefaultTrainSpacing
		}
		if trainLen := time.Duration(trainCount) * effSpacing; trainLen >= timeout {
			problems = append(problems, fmt.Sprintf("train of %d × %s (%s) must fit inside %s (%s)",
				trainCount, effSpacing, trainLen, f.Timeout, timeout))
		}
	case trainType(t):
		// No explicit train, but the prober still runs one built from the
		// defaults, so the implicit train needs the same budget check.
		if trainLen := DefaultTrainCount * DefaultTrainSpacing; trainLen >= timeout {
			problems = append(problems, fmt.Sprintf("%s's default train of %d × %s (%s) must fit inside %s (%s); raise %s or set %s/%s",
				TypeName(int16(t)), DefaultTrainCount, DefaultTrainSpacing, trainLen, f.Timeout, timeout,
				f.Timeout, f.TrainCount, f.TrainSpacing))
		}
	}
	return problems
}

// ParamKind tells a form (or validator) how to treat a param value.
type ParamKind string

const (
	KindString ParamKind = "string" // non-empty free text
	KindPort   ParamKind = "port"   // integer 1–65535
	KindBool   ParamKind = "bool"   // "true" or "false"
	KindEnum   ParamKind = "enum"   // one of Enum (case-insensitive)
	KindStatus ParamKind = "status" // exact HTTP status ("200") or class ("2xx")
)

// ParamSpec describes one type-specific param key. The registry is the only
// machine-readable statement of what the agent probers actually read —
// an unknown key silently no-ops on the agent, so writes reject anything
// not listed here (fail loud).
type ParamSpec struct {
	Key            string    `json:"key"`
	Hint           string    `json:"hint"`
	Kind           ParamKind `json:"kind"`
	Enum           []string  `json:"enum,omitempty"`
	RequiredMesh   bool      `json:"required_mesh,omitempty"`
	RequiredDirect bool      `json:"required_direct,omitempty"`
	MeshOnly       bool      `json:"mesh_only,omitempty"`
}

// meshPortSpec: mesh templates have no target row to carry a port, so tcp
// and tls templates take it as a param (read by meshexpand only) — direct
// probes must not set it because their target row already carries one.
var meshPortSpec = ParamSpec{
	Key: "port", Kind: KindPort, RequiredMesh: true, MeshOnly: true,
	Hint: "peer port for mesh templates",
}

// registry mirrors exactly what the probers read; keep in lockstep with
// internal/agent/probes/{tls,http,dns,ntp}.go and meshexpand's meshPort.
var registry = map[pb.ProbeType][]ParamSpec{
	pb.ProbeType_PROBE_TYPE_ICMP: {},
	pb.ProbeType_PROBE_TYPE_TCP:  {meshPortSpec},
	pb.ProbeType_PROBE_TYPE_TLS: {
		meshPortSpec,
		{Key: "tls.sni", Kind: KindString, Hint: "override the handshake server name (default: target host)"},
		{Key: "tls.insecure_skip_verify", Kind: KindBool, Hint: "skip certificate verification"},
	},
	pb.ProbeType_PROBE_TYPE_HTTP: {
		{Key: "http.method", Kind: KindString, Hint: "request method (default GET)"},
		{Key: "http.expect_status", Kind: KindStatus, Hint: `expected status: exact ("200") or class ("2xx"); default 200`},
		{Key: "http.insecure_skip_verify", Kind: KindBool, Hint: "skip certificate verification (self-signed endpoints)"},
	},
	pb.ProbeType_PROBE_TYPE_DNS: {
		{Key: "dns.qname", Kind: KindString, RequiredMesh: true, RequiredDirect: true, Hint: "name to query"},
		{Key: "dns.qtype", Kind: KindEnum, Hint: "query type (default A)",
			Enum: []string{"A", "AAAA", "CNAME", "MX", "NS", "PTR", "SOA", "SRV", "TXT"}},
		{Key: "dns.expect_rcode", Kind: KindEnum, Hint: "expected RCODE (default NOERROR)",
			Enum: []string{"NOERROR", "FORMERR", "SERVFAIL", "NXDOMAIN", "NOTIMPL", "REFUSED"}},
		{Key: "dns.resolver", Kind: KindString, Hint: "override resolver host:port (default: the target)"},
	},
	pb.ProbeType_PROBE_TYPE_NTP:        {},
	pb.ProbeType_PROBE_TYPE_TRACEROUTE: {},
}

// Params returns the registry entry for a type (nil for unknown types —
// callers reach this only after ParseType).
func Params(t pb.ProbeType) []ParamSpec {
	return registry[t]
}

// directOnlyReason lists types that cannot be mesh templates, keyed to the
// reason spliced into the rejection message. HTTP: the prober reads only
// Target.Url, and mesh expansion carries only the peer's address/port — an
// expanded HTTP mesh probe would fail on an empty URL every run. NTP: peer
// agents do not run NTP servers, so an expanded template would query port
// 123 on hosts that serve nothing.
var directOnlyReason = map[pb.ProbeType]string{
	pb.ProbeType_PROBE_TYPE_HTTP: "mesh expansion carries only the peer address/port and the prober needs a URL",
	pb.ProbeType_PROBE_TYPE_NTP:  "peer agents do not run NTP servers, so an expanded template would query port 123 on hosts that serve nothing",
}

// DirectOnly reports types that cannot be mesh templates.
func DirectOnly(t pb.ProbeType) bool {
	_, ok := directOnlyReason[t]
	return ok
}

// MinMeshMembers is the smallest mesh that can produce any probe at all.
// Expansion walks ordered pairs of DISTINCT member sites, so a mesh with one
// member expands to nothing: agents receive an empty probe list, no result
// is ever recorded, and every dashboard reads "none yet" forever with no
// error anywhere to explain it. Write time is the only honest place to say so.
const MinMeshMembers = 2

// ValidateMeshMembers rejects a mesh probe on a mesh too small to expand.
// Callers supply the current member count; the rule and its wording live
// here so the CLI and the API refuse identically.
func ValidateMeshMembers(meshName string, members int) []string {
	if members >= MinMeshMembers {
		return nil
	}
	noun := "members"
	if members == 1 {
		noun = "member"
	}
	return []string{fmt.Sprintf(
		"mesh %q has %d %s: a mesh probe expands over ordered pairs of distinct sites, so it needs at least %d before it can produce a single probe",
		meshName, members, noun, MinMeshMembers)}
}

// ValidateMeshMemberRemoval guards the other route to a dead mesh probe:
// creation refuses a mesh that is already too small, but removing a member
// from a probed two-site mesh reaches the same end state — templates that
// expand to nothing, agents with an empty probe list, dashboards reading
// "none yet" with no error anywhere. Refused only when templates would be
// stranded; emptying an unprobed mesh is ordinary housekeeping.
func ValidateMeshMemberRemoval(meshName string, membersAfter, templates int) []string {
	if templates == 0 || membersAfter >= MinMeshMembers {
		return nil
	}
	noun := "templates"
	if templates == 1 {
		noun = "template"
	}
	return []string{fmt.Sprintf(
		"removing this site would leave mesh %q with %d member(s) and %d probe %s that can no longer expand; delete the probe %s first, or keep at least %d members",
		meshName, membersAfter, templates, noun, noun, MinMeshMembers)}
}

// NTPRecommendedMinInterval is the fastest cadence that is polite to NTP
// servers the operator does not run: public servers and pool members
// rate-limit faster pollers, answering with kiss-o'-death RATE.
const NTPRecommendedMinInterval = 60 * time.Second

// Warnings reports configurations that are valid, will run, and are almost
// certainly not what the operator meant. They never block a write — each
// case has a legitimate use — so callers surface them alongside success.
func Warnings(t pb.ProbeType, mesh bool, interval time.Duration, params map[string]string) []string {
	var out []string
	// A mesh dns probe has no target row: expansion points it at the peer
	// AGENT's probe address, so it queries another agent's host on port 53
	// as though it were a resolver. That is deliberate only when the agent
	// hosts really do run resolvers; otherwise every run fails against a
	// port nothing is listening on.
	if mesh && t == pb.ProbeType_PROBE_TYPE_DNS && params["dns.resolver"] == "" {
		out = append(out, "mesh dns probes query each peer agent's own address on port 53 as if it were a resolver; "+
			`set "dns.resolver" to the resolver you mean to test, or use a direct probe against a resolver target, `+
			"unless your agent hosts genuinely serve DNS")
	}
	// Legitimate when you operate the time server yourself; public servers
	// answer over-eager pollers with kiss-o'-death RATE, which the prober
	// reports as an error every run.
	if t == pb.ProbeType_PROBE_TYPE_NTP && interval > 0 && interval < NTPRecommendedMinInterval {
		out = append(out, fmt.Sprintf("ntp probes more frequent than %s risk rate limiting (kiss-o'-death RATE) from public NTP servers; "+
			"use an interval of at least %s unless you operate the server", NTPRecommendedMinInterval, NTPRecommendedMinInterval))
	}
	return out
}

// ValidateParams checks a param map against the registry for the given type
// and assignment mode, returning every problem.
func ValidateParams(t pb.ProbeType, mesh bool, params map[string]string) []string {
	var problems []string
	if mesh && DirectOnly(t) {
		problems = append(problems,
			fmt.Sprintf("%s probes cannot be mesh templates: %s", TypeName(int16(t)), directOnlyReason[t]))
	}
	specs := registry[t]
	byKey := make(map[string]ParamSpec, len(specs))
	accepted := make([]string, 0, len(specs))
	for _, s := range specs {
		byKey[s.Key] = s
		accepted = append(accepted, s.Key)
	}

	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		spec, ok := byKey[k]
		if !ok {
			acceptedText := "none"
			if len(accepted) > 0 {
				acceptedText = strings.Join(accepted, ", ")
			}
			problems = append(problems, fmt.Sprintf("params: unknown key %q for probe type %s (accepted: %s)",
				k, TypeName(int16(t)), acceptedText))
			continue
		}
		if spec.MeshOnly && !mesh {
			problems = append(problems, fmt.Sprintf("params: %q applies only to mesh probes (direct targets carry their own port)", k))
			continue
		}
		if p := validateValue(spec, params[k]); p != "" {
			problems = append(problems, p)
		}
	}

	for _, s := range specs {
		required := (mesh && s.RequiredMesh) || (!mesh && s.RequiredDirect)
		if required && params[s.Key] == "" {
			mode := ""
			if s.MeshOnly {
				mode = "mesh "
			}
			problems = append(problems, fmt.Sprintf("params: %q is required for %s%s probes", s.Key, mode, TypeName(int16(t))))
		}
	}
	return problems
}

func validateValue(spec ParamSpec, v string) string {
	switch spec.Kind {
	case KindPort:
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Sprintf("params: %q must be an integer between 1 and 65535, got %q", spec.Key, v)
		}
	case KindBool:
		if v != "true" && v != "false" {
			return fmt.Sprintf(`params: %q must be "true" or "false", got %q`, spec.Key, v)
		}
	case KindEnum:
		if !slices.Contains(spec.Enum, strings.ToUpper(v)) {
			return fmt.Sprintf("params: unsupported %s %q (accepted: %s)", spec.Key, v, strings.Join(spec.Enum, ", "))
		}
	case KindStatus:
		ok := len(v) == 3 && v[0] >= '1' && v[0] <= '5' &&
			((v[1] == 'x' && v[2] == 'x') || (v[1] >= '0' && v[1] <= '9' && v[2] >= '0' && v[2] <= '9'))
		if !ok {
			return fmt.Sprintf(`params: %q must be an exact status ("200") or a class ("2xx"), got %q`, spec.Key, v)
		}
	case KindString:
		if strings.TrimSpace(v) == "" {
			return fmt.Sprintf("params: %q must not be empty", spec.Key)
		}
		if spec.Key == "http.method" && strings.ContainsAny(v, " \t") {
			return fmt.Sprintf("params: %q must be a single HTTP method token, got %q", spec.Key, v)
		}
	}
	return ""
}
