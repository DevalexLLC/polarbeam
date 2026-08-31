package probes

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"google.golang.org/protobuf/types/known/durationpb"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
)

func TestPathHash(t *testing.T) {
	hop := func(ttl uint32, addrs ...string) *pb.Hop { return &pb.Hop{Ttl: ttl, Addrs: addrs} }

	base := pathHash([]*pb.Hop{hop(1, "10.0.0.1"), hop(2, "10.0.0.2")})
	if len(base) != 32 {
		t.Fatalf("hash length = %d, want 32", len(base))
	}

	// Address order within a hop must not matter (sorted before hashing)...
	a := pathHash([]*pb.Hop{hop(1, "10.0.0.1", "10.0.0.9")})
	b := pathHash([]*pb.Hop{hop(1, "10.0.0.9", "10.0.0.1")})
	if !bytes.Equal(a, b) {
		t.Error("addr order within a hop changed the hash")
	}
	// ...and duplicates must not either.
	c := pathHash([]*pb.Hop{hop(1, "10.0.0.1", "10.0.0.1", "10.0.0.9")})
	if !bytes.Equal(a, c) {
		t.Error("duplicate addrs changed the hash")
	}

	// Hop order matters.
	fwd := pathHash([]*pb.Hop{hop(1, "10.0.0.1"), hop(2, "10.0.0.2")})
	rev := pathHash([]*pb.Hop{hop(1, "10.0.0.2"), hop(2, "10.0.0.1")})
	if bytes.Equal(fwd, rev) {
		t.Error("hop order did not change the hash")
	}

	// Silent hops hash as "*" and are distinct from responding hops.
	silent := pathHash([]*pb.Hop{hop(1), hop(2, "10.0.0.2")})
	if bytes.Equal(silent, fwd) {
		t.Error("silent hop hashed like a responding hop")
	}
}

func TestTraceroutePortMapping(t *testing.T) {
	for ttl := 1; ttl <= tracerouteMaxHops; ttl++ {
		for idx := 0; idx < tracerouteProbesPerHop; idx++ {
			gotTTL, gotIdx, ok := tracerouteFromPort(tracerouteDstPort(ttl, idx))
			if !ok || gotTTL != ttl || gotIdx != idx {
				t.Fatalf("round trip (%d,%d) -> (%d,%d,%v)", ttl, idx, gotTTL, gotIdx, ok)
			}
		}
	}
	for _, port := range []int{0, tracerouteBasePort - 1, tracerouteBasePort + tracerouteMaxHops*tracerouteProbesPerHop} {
		if _, _, ok := tracerouteFromPort(port); ok {
			t.Errorf("port %d must not decode", port)
		}
	}
}

// quotedV4 builds the quoted payload of an ICMP error: the original IPv4
// header plus the first 8 bytes of the UDP header.
func quotedV4(srcPort, dstPort int, dst net.IP) []byte {
	q := make([]byte, 28)
	q[0] = 0x45 // version 4, IHL 5
	copy(q[16:20], dst.To4())
	binary.BigEndian.PutUint16(q[20:22], uint16(srcPort))
	binary.BigEndian.PutUint16(q[22:24], uint16(dstPort))
	return q
}

func TestParseTracerouteReply(t *testing.T) {
	target := net.ParseIP("192.0.2.7")
	local := 54321

	marshal := func(m icmp.Message) []byte {
		b, err := m.Marshal(nil)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	// Time-exceeded for (ttl 4, idx 1).
	te := marshal(icmp.Message{
		Type: ipv4.ICMPTypeTimeExceeded,
		Body: &icmp.TimeExceeded{Data: quotedV4(local, tracerouteDstPort(4, 1), target)},
	})
	ttl, idx, reached, ok := parseTracerouteReply(true, te, local, target)
	if !ok || ttl != 4 || idx != 1 || reached {
		t.Errorf("time-exceeded parse = (%d,%d,%v,%v), want (4,1,false,true)", ttl, idx, reached, ok)
	}

	// Port-unreachable (code 3) marks the destination reached.
	pu := marshal(icmp.Message{
		Type: ipv4.ICMPTypeDestinationUnreachable, Code: 3,
		Body: &icmp.DstUnreach{Data: quotedV4(local, tracerouteDstPort(1, 0), target)},
	})
	ttl, idx, reached, ok = parseTracerouteReply(true, pu, local, target)
	if !ok || ttl != 1 || idx != 0 || !reached {
		t.Errorf("port-unreachable parse = (%d,%d,%v,%v), want (1,0,true,true)", ttl, idx, reached, ok)
	}

	// A quoted packet for someone else's flow must be rejected.
	other := marshal(icmp.Message{
		Type: ipv4.ICMPTypeTimeExceeded,
		Body: &icmp.TimeExceeded{Data: quotedV4(local+1, tracerouteDstPort(1, 0), target)},
	})
	if _, _, _, ok := parseTracerouteReply(true, other, local, target); ok {
		t.Error("reply quoting a foreign source port must not match")
	}

	// A quoted packet to a different destination must be rejected.
	wrongDst := marshal(icmp.Message{
		Type: ipv4.ICMPTypeTimeExceeded,
		Body: &icmp.TimeExceeded{Data: quotedV4(local, tracerouteDstPort(1, 0), net.ParseIP("198.51.100.9"))},
	})
	if _, _, _, ok := parseTracerouteReply(true, wrongDst, local, target); ok {
		t.Error("reply quoting a foreign destination must not match")
	}
}

// Preserved historic behavior: traceroute matches on the quoted ports and
// destination alone and has never checked the quoted protocol byte
// (quotedV4 leaves it zero, so every passing case above already relies on
// this). pathmtu DOES check it; this pins the asymmetry so it cannot be
// "fixed" in passing through the shared quotedInner helper.
func TestParseTracerouteReplyIgnoresQuotedProto(t *testing.T) {
	target := net.ParseIP("192.0.2.7")
	local := 54321
	q := quotedV4(local, tracerouteDstPort(2, 0), target)
	q[9] = 6 // TCP, not UDP — still ours by port and destination
	b, err := (&icmp.Message{Type: ipv4.ICMPTypeTimeExceeded, Body: &icmp.TimeExceeded{Data: q}}).Marshal(nil)
	if err != nil {
		t.Fatal(err)
	}
	ttl, idx, _, ok := parseTracerouteReply(true, b, local, target)
	if !ok || ttl != 2 || idx != 0 {
		t.Errorf("quoted proto byte must be ignored: (%d,%d,%v)", ttl, idx, ok)
	}
}

func TestTracerouteLoopback(t *testing.T) {
	raw, err := icmp.ListenPacket("ip4:icmp", "")
	if err != nil {
		t.Skipf("no raw ICMP socket (needs CAP_NET_RAW): %v", err)
	}
	raw.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	spec := &pb.ProbeSpec{
		ProbeId: "test-traceroute",
		Type:    pb.ProbeType_PROBE_TYPE_TRACEROUTE,
		Target: &pb.Target{
			Kind:     pb.TargetKind_TARGET_KIND_EXTERNAL,
			TargetId: "test-target",
			Address:  "127.0.0.1",
		},
		Timeout: durationpb.New(5 * time.Second),
	}
	res := Traceroute{}.Run(ctx, spec)
	if res.Status != pb.ProbeStatus_PROBE_STATUS_OK {
		t.Fatalf("status = %v, error = %q", res.Status, res.Error)
	}
	tr := res.Traceroute
	if tr == nil || !tr.DestReached {
		t.Fatalf("traceroute result missing or dest not reached: %+v", tr)
	}
	if len(tr.Hops) != 1 || len(tr.Hops[0].Addrs) == 0 || tr.Hops[0].Addrs[0] != "127.0.0.1" {
		t.Errorf("loopback trace must be one hop via 127.0.0.1: %+v", tr.Hops)
	}
	if len(tr.PathHash) != 32 {
		t.Errorf("path_hash length = %d, want 32", len(tr.PathHash))
	}
	// Run-level accounting only; hop timings never leak into the row fields.
	if res.Sent != 1 || res.Received != 1 {
		t.Errorf("sent/received = %d/%d, want 1/1", res.Sent, res.Received)
	}
	if res.Timings.TotalUs != -1 || res.Rtt.AvgUs != -1 {
		t.Errorf("row timings must stay unmeasured: %+v / %+v", res.Timings, res.Rtt)
	}
}
