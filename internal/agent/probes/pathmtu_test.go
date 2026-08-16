package probes

import (
	"context"
	"encoding/binary"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"google.golang.org/protobuf/types/known/durationpb"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
)

// drivePMTU runs the search against an oracle until it terminates and
// returns how many sizes were tested.
func drivePMTU(t *testing.T, s *pmtuSearch, oracle func(size int) (sizeVerdict, int)) int {
	t.Helper()
	steps := 0
	for {
		size, more := s.next()
		if !more {
			return steps
		}
		if steps++; steps > 40 {
			t.Fatal("search did not terminate")
		}
		v, adv := oracle(size)
		s.record(size, v, adv)
	}
}

func TestPMTUSearchCleanPath(t *testing.T) {
	s := newPMTUSearch(1280, 1500, false)
	steps := drivePMTU(t, s, func(size int) (sizeVerdict, int) { return verdictOK, 0 })
	if steps != 1 {
		t.Errorf("clean path took %d probes, want 1 (max only)", steps)
	}
	o := s.outcome()
	if o.largestOK != 1500 || o.smallestFailed != 0 || o.blackHole || o.localConstraint {
		t.Errorf("outcome = %+v, want largestOK=1500 smallestFailed=0", o)
	}
}

func TestPMTUSearchBinarySearch(t *testing.T) {
	// True MTU 1400, failures answered with a FragNeeded carrying no
	// plausible MTU — forces a pure bisect.
	s := newPMTUSearch(1280, 1500, false)
	steps := drivePMTU(t, s, func(size int) (sizeVerdict, int) {
		if size <= 1400 {
			return verdictOK, 0
		}
		return verdictTooBig, 0
	})
	o := s.outcome()
	if o.largestOK != 1400 || o.smallestFailed != 1401 {
		t.Errorf("outcome = %+v, want largestOK=1400 smallestFailed=1401", o)
	}
	if o.blackHole || o.nextHopMTU != 0 {
		t.Errorf("bisect outcome flags wrong: %+v", o)
	}
	if steps > 15 {
		t.Errorf("bisect took %d probes, want <= 15", steps)
	}
}

func TestPMTUSearchPTBJump(t *testing.T) {
	// The FragNeeded for max advertises 1400; the search must test the
	// advertised value next and stop once it verifies, never probing min.
	s := newPMTUSearch(1280, 1500, false)
	var sizes []int
	drivePMTU(t, s, func(size int) (sizeVerdict, int) {
		sizes = append(sizes, size)
		if size <= 1400 {
			return verdictOK, 0
		}
		return verdictTooBig, 1400
	})
	if len(sizes) != 2 || sizes[0] != 1500 || sizes[1] != 1400 {
		t.Fatalf("probed sizes = %v, want [1500 1400]", sizes)
	}
	o := s.outcome()
	if o.largestOK != 1400 || o.smallestFailed != 1500 || o.nextHopMTU != 1400 {
		t.Errorf("outcome = %+v, want largestOK=1400 smallestFailed=1500 nextHopMTU=1400", o)
	}
}

func TestPMTUSearchImplausiblePTB(t *testing.T) {
	// Advertised MTUs of 0 (absent) and >= probe size still bound the
	// bracket but must not be reported or jumped to.
	s := newPMTUSearch(1280, 1500, false)
	s.record(1500, verdictTooBig, 0)
	s.record(1390, verdictTooBig, 2000)
	if s.nextHopMTU != 0 || s.override != 0 {
		t.Errorf("implausible advertised MTU recorded: nextHop=%d override=%d", s.nextHopMTU, s.override)
	}
	// An advertised MTU below the configured floor is reported but never
	// probed (probing below mtu.min is out of bounds).
	s2 := newPMTUSearch(1400, 1500, false)
	s2.record(1500, verdictTooBig, 1200)
	if s2.nextHopMTU != 1200 || s2.override != 0 {
		t.Errorf("below-floor advertised MTU: nextHop=%d override=%d, want 1200/0", s2.nextHopMTU, s2.override)
	}
}

func TestPMTUSearchBlackHole(t *testing.T) {
	s := newPMTUSearch(1280, 1500, false)
	drivePMTU(t, s, func(size int) (sizeVerdict, int) {
		if size <= 1400 {
			return verdictOK, 0
		}
		return verdictSilent, 0
	})
	o := s.outcome()
	if !o.blackHole || o.largestOK != 1400 || o.smallestFailed != 1401 {
		t.Errorf("outcome = %+v, want black hole with largestOK=1400", o)
	}
}

func TestPMTUSearchLocalConstraint(t *testing.T) {
	// Local interface MTU 1400: sends above it fail with EMSGSIZE.
	s := newPMTUSearch(1280, 1500, false)
	drivePMTU(t, s, func(size int) (sizeVerdict, int) {
		if size <= 1400 {
			return verdictOK, 0
		}
		return verdictLocal, 0
	})
	o := s.outcome()
	if !o.localConstraint || o.blackHole || o.largestOK != 1400 {
		t.Errorf("outcome = %+v, want local constraint with largestOK=1400", o)
	}
}

func TestPMTUSearchMinSilent(t *testing.T) {
	s := newPMTUSearch(1280, 1500, false)
	steps := drivePMTU(t, s, func(size int) (sizeVerdict, int) { return verdictSilent, 0 })
	if steps != 2 {
		t.Errorf("all-silent path took %d probes, want 2 (max then min)", steps)
	}
	o := s.outcome()
	if o.largestOK != 0 || o.smallestFailed != 1280 || o.blackHole {
		t.Errorf("outcome = %+v, want largestOK=0 smallestFailed=1280 and no black-hole flag", o)
	}
}

func TestPMTUSearchMonotonicGuards(t *testing.T) {
	s := newPMTUSearch(1280, 1500, false)
	s.record(1500, verdictSilent, 0) // hi=1500
	s.record(1280, verdictOK, 0)     // lo=1280
	// Late/contradictory evidence must never widen the bracket.
	s.record(1280, verdictOK, 0)     // duplicate OK: no change
	s.record(1200, verdictOK, 0)     // below lo: no change
	s.record(1600, verdictSilent, 0) // above hi: no change
	s.record(1500, verdictOK, 0)     // OK at the failed bound: ignored
	if s.lo != 1280 || s.hi != 1500 || s.cause != causeSilent {
		t.Errorf("bracket = [%d,%d) cause=%v, want [1280,1500) silent", s.lo, s.hi, s.cause)
	}
	// A tighter failure supersedes the bound and its cause.
	s.record(1400, verdictTooBig, 1390)
	if s.hi != 1400 || s.cause != causeTooBig || s.nextHopMTU != 1390 {
		t.Errorf("after PTB: hi=%d cause=%v nextHop=%d, want 1400/tooBig/1390", s.hi, s.cause, s.nextHopMTU)
	}
}

func mustMarshalICMP(t *testing.T, m icmp.Message) []byte {
	t.Helper()
	b, err := m.Marshal(nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// quotedEchoV4 builds the quoted payload of an ICMPv4 error: the original
// IPv4 header plus the 8-byte echo header (all RFC 792 requires).
func quotedEchoV4(id, seq int, dst net.IP) []byte {
	q := make([]byte, 28)
	q[0] = 0x45
	q[9] = protoICMP
	copy(q[16:20], dst.To4())
	e := q[20:28]
	e[0] = 8 // echo request
	binary.BigEndian.PutUint16(e[4:6], uint16(id))
	binary.BigEndian.PutUint16(e[6:8], uint16(seq))
	return q
}

// quotedEchoV6 builds the quoted payload of an ICMPv6 error: the original
// IPv6 header plus the 8-byte echo header.
func quotedEchoV6(id, seq int, dst net.IP) []byte {
	q := make([]byte, 48)
	q[0] = 0x60
	q[6] = protoICMPv6
	copy(q[24:40], dst.To16())
	e := q[40:48]
	e[0] = 128 // echo request
	binary.BigEndian.PutUint16(e[4:6], uint16(id))
	binary.BigEndian.PutUint16(e[6:8], uint16(seq))
	return q
}

// fragNeededV4 marshals a Fragmentation Needed message and plants the
// next-hop MTU in the second header word (x/net's DstUnreach cannot carry
// it; ParseMessage does not verify checksums, so patching is safe).
func fragNeededV4(t *testing.T, id, seq, mtu int, dst net.IP) []byte {
	t.Helper()
	b := mustMarshalICMP(t, icmp.Message{
		Type: ipv4.ICMPTypeDestinationUnreachable, Code: 4,
		Body: &icmp.DstUnreach{Data: quotedEchoV4(id, seq, dst)},
	})
	binary.BigEndian.PutUint16(b[6:8], uint16(mtu))
	return b
}

func TestParsePMTUReplyEcho(t *testing.T) {
	target := net.ParseIP("192.0.2.7")
	token := []byte("tokentok")
	id := 4242

	payload := make([]byte, 100)
	copy(payload, token)
	reply := mustMarshalICMP(t, icmp.Message{
		Type: ipv4.ICMPTypeEchoReply,
		Body: &icmp.Echo{ID: id, Seq: 7, Data: payload},
	})
	kind, seq, mtu := parsePMTUReply(false, reply, id, token, target)
	if kind != replyEcho || seq != 7 || mtu != 0 {
		t.Errorf("v4 echo parse = (%v,%d,%d), want (echo,7,0)", kind, seq, mtu)
	}

	target6 := net.ParseIP("2001:db8::7")
	reply6 := mustMarshalICMP(t, icmp.Message{
		Type: ipv6.ICMPTypeEchoReply,
		Body: &icmp.Echo{ID: id, Seq: 9, Data: payload},
	})
	kind, seq, _ = parsePMTUReply(true, reply6, id, token, target6)
	if kind != replyEcho || seq != 9 {
		t.Errorf("v6 echo parse = (%v,%d), want (echo,9)", kind, seq)
	}

	// Wrong ID and wrong token must be rejected.
	if k, _, _ := parsePMTUReply(false, reply, id+1, token, target); k != replyNone {
		t.Error("echo reply with foreign ID must not match")
	}
	if k, _, _ := parsePMTUReply(false, reply, id, []byte("other-t0"), target); k != replyNone {
		t.Error("echo reply with foreign token must not match")
	}
}

func TestParsePMTUReplyFragNeeded(t *testing.T) {
	target := net.ParseIP("192.0.2.7")
	token := []byte("tokentok")
	id := 4242

	b := fragNeededV4(t, id, 5, 1400, target)
	kind, seq, mtu := parsePMTUReply(false, b, id, token, target)
	if kind != replyTooBig || seq != 5 || mtu != 1400 {
		t.Errorf("frag-needed parse = (%v,%d,%d), want (tooBig,5,1400)", kind, seq, mtu)
	}
}

func TestParsePMTUReplyPTB(t *testing.T) {
	target := net.ParseIP("2001:db8::7")
	token := []byte("tokentok")
	id := 4242

	b := mustMarshalICMP(t, icmp.Message{
		Type: ipv6.ICMPTypePacketTooBig,
		Body: &icmp.PacketTooBig{MTU: 1450, Data: quotedEchoV6(id, 3, target)},
	})
	kind, seq, mtu := parsePMTUReply(true, b, id, token, target)
	if kind != replyTooBig || seq != 3 || mtu != 1450 {
		t.Errorf("PTB parse = (%v,%d,%d), want (tooBig,3,1450)", kind, seq, mtu)
	}
}

func TestParsePMTUReplyMalformed(t *testing.T) {
	target := net.ParseIP("192.0.2.7")
	target6 := net.ParseIP("2001:db8::7")
	token := []byte("tokentok")
	id := 4242

	truncatedQuote := mustMarshalICMP(t, icmp.Message{
		Type: ipv4.ICMPTypeDestinationUnreachable, Code: 4,
		Body: &icmp.DstUnreach{Data: quotedEchoV4(id, 1, target)[:12]},
	})
	wrongCode := mustMarshalICMP(t, icmp.Message{
		Type: ipv4.ICMPTypeDestinationUnreachable, Code: 3,
		Body: &icmp.DstUnreach{Data: quotedEchoV4(id, 1, target)},
	})
	wrongDst := fragNeededV4(t, id, 1, 1400, net.ParseIP("198.51.100.9"))
	wrongID := fragNeededV4(t, id+1, 1, 1400, target)
	notICMPQuote := quotedEchoV4(id, 1, target)
	notICMPQuote[9] = 17 // quoted protocol UDP, not ICMP
	wrongProto := mustMarshalICMP(t, icmp.Message{
		Type: ipv4.ICMPTypeDestinationUnreachable, Code: 4,
		Body: &icmp.DstUnreach{Data: notICMPQuote},
	})
	shortPTB := mustMarshalICMP(t, icmp.Message{
		Type: ipv6.ICMPTypePacketTooBig,
		Body: &icmp.PacketTooBig{MTU: 1450, Data: quotedEchoV6(id, 1, target6)[:30]},
	})

	v4Cases := map[string][]byte{
		"truncated quote": truncatedQuote,
		"wrong code":      wrongCode,
		"wrong dst":       wrongDst,
		"wrong id":        wrongID,
		"non-ICMP quote":  wrongProto,
		"garbage":         {0x03, 0x04},
	}
	for name, b := range v4Cases {
		if kind, _, _ := parsePMTUReply(false, b, id, token, target); kind != replyNone {
			t.Errorf("%s must parse as none, got %v", name, kind)
		}
	}
	if kind, _, _ := parsePMTUReply(true, shortPTB, id, token, target6); kind != replyNone {
		t.Errorf("short PTB must parse as none, got %v", kind)
	}
}

func pmtuSpec(address string, params map[string]string) *pb.ProbeSpec {
	return &pb.ProbeSpec{
		ProbeId: "test-path-mtu",
		Type:    pb.ProbeType_PROBE_TYPE_PATH_MTU,
		Target: &pb.Target{
			Kind:     pb.TargetKind_TARGET_KIND_EXTERNAL,
			TargetId: "test-target",
			Address:  address,
		},
		Timeout: durationpb.New(5 * time.Second),
		Params:  params,
	}
}

func TestPathMTUBadParams(t *testing.T) {
	cases := []map[string]string{
		{"mtu.min": "abc"},
		{"mtu.max": "1e4"},
		{"mtu.min": "10"},
		{"mtu.max": "99999"},
		{"mtu.min": "1400", "mtu.max": "1300"},
		{"mtu.family": "5"},
	}
	for _, params := range cases {
		res := PathMTU{}.Run(context.Background(), pmtuSpec("127.0.0.1", params))
		if res.Status != pb.ProbeStatus_PROBE_STATUS_ERROR || res.Error == "" {
			t.Errorf("params %v: status = %v (%q), want ERROR with message", params, res.Status, res.Error)
		}
	}
}

func TestPathMTUResolveFailure(t *testing.T) {
	res := PathMTU{}.Run(context.Background(), pmtuSpec("host.invalid", nil))
	if res.Status != pb.ProbeStatus_PROBE_STATUS_DNS_FAILURE {
		t.Errorf("status = %v (%q), want DNS_FAILURE", res.Status, res.Error)
	}
}

func TestPathMTURunLoopback(t *testing.T) {
	if err := tryPathMTU(); err != nil {
		t.Skipf("no raw ICMP + PMTU-probe socket (needs CAP_NET_RAW on Linux): %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res := PathMTU{}.Run(ctx, pmtuSpec("127.0.0.1", nil))
	if res.Status != pb.ProbeStatus_PROBE_STATUS_OK {
		t.Fatalf("status = %v, error = %q", res.Status, res.Error)
	}
	m := res.PathMtu
	if m == nil {
		t.Fatal("path_mtu payload missing")
	}
	// Loopback MTU is far above 1500, so the default max passes cleanly.
	if m.LargestOkBytes != defaultMTUMax || m.SmallestFailedBytes != 0 || m.BlackHoleSuspected {
		t.Errorf("payload = %+v, want largest_ok=%d smallest_failed=0", m, defaultMTUMax)
	}
	if m.IpVersion != 4 || m.RttUs < 0 {
		t.Errorf("ip_version/rtt = %d/%d, want 4 and a measured RTT", m.IpVersion, m.RttUs)
	}
	// Run accounting and latency families must stay untouched: the size
	// sweep is not a train and PMTU RTTs never join the rtt columns.
	if res.Sent != 0 || res.Received != 0 {
		t.Errorf("sent/received = %d/%d, want 0/0", res.Sent, res.Received)
	}
	if res.Rtt.AvgUs != -1 || res.Timings.TotalUs != -1 {
		t.Errorf("row timings must stay unmeasured: %+v / %+v", res.Rtt, res.Timings)
	}
}

func TestPathMTURunNoCapability(t *testing.T) {
	if _, err := net.ListenPacket("ip4:icmp", ""); err == nil {
		t.Skip("raw ICMP available; cannot exercise the missing-capability path")
	}
	res := PathMTU{}.Run(context.Background(), pmtuSpec("127.0.0.1", nil))
	if res.Status != pb.ProbeStatus_PROBE_STATUS_ERROR || !strings.Contains(res.Error, "CAP_NET_RAW") {
		t.Errorf("status = %v (%q), want ERROR naming CAP_NET_RAW", res.Status, res.Error)
	}
}

func TestSelfCheckListsPathMTU(t *testing.T) {
	for _, c := range SelfCheck(t.TempDir()) {
		if c.Name == "path_mtu" {
			if c.Fatal {
				t.Error("path_mtu selfcheck must not be fatal")
			}
			return
		}
	}
	t.Error("selfcheck output has no path_mtu check")
}
