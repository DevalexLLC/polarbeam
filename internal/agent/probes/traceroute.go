package probes

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
)

const (
	tracerouteMaxHops      = 30
	tracerouteProbesPerHop = 3
	tracerouteBasePort     = 33434
	traceroutePerHopWait   = time.Second
)

// Traceroute sends UDP probes with incrementing TTL and reads ICMP
// time-exceeded / port-unreachable on a raw ICMP socket. Unprivileged
// datagram ICMP sockets do not deliver errors elicited by another socket's
// packets, so CAP_NET_RAW is a hard requirement; its absence is an ERROR
// result every cadence, never a silent skip.
type Traceroute struct{}

// tracerouteEvent is one matched ICMP error from the reader: which probe
// (ttl, idx) elicited it, from whom, and whether it proves arrival.
type tracerouteEvent struct {
	ttl, idx    int
	peer        string
	at          time.Time
	destReached bool
}

func (Traceroute) Run(ctx context.Context, spec *pb.ProbeSpec) *pb.ProbeResult {
	start := time.Now()
	res := newResult(spec, start)
	res.Sent = 1
	deadline := runDeadline(ctx, start, 30*time.Second)

	ip, err := resolveIP(ctx, spec.GetTarget().GetAddress())
	if err != nil {
		return fail(res, err, pb.ProbeStatus_PROBE_STATUS_UNSPECIFIED)
	}
	v4 := ip.To4() != nil

	rawNet, udpNet := "ip4:icmp", "udp4"
	if !v4 {
		rawNet, udpNet = "ip6:ipv6-icmp", "udp6"
	}
	rawConn, err := icmp.ListenPacket(rawNet, "")
	if err != nil {
		res.Status = pb.ProbeStatus_PROBE_STATUS_ERROR
		res.Error = fmt.Sprintf("traceroute requires CAP_NET_RAW (raw ICMP socket): %v", err)
		return res
	}
	defer rawConn.Close()

	udpConn, err := net.ListenPacket(udpNet, ":0")
	if err != nil {
		return fail(res, err, pb.ProbeStatus_PROBE_STATUS_UNSPECIFIED)
	}
	defer udpConn.Close()
	localPort := udpConn.LocalAddr().(*net.UDPAddr).Port

	var setTTL func(int) error
	if v4 {
		pc := ipv4.NewPacketConn(udpConn)
		setTTL = pc.SetTTL
	} else {
		pc := ipv6.NewPacketConn(udpConn)
		setTTL = pc.SetHopLimit
	}

	parse := func(pkt []byte, peer net.Addr, at time.Time) (tracerouteEvent, bool) {
		ttl, idx, reached, ok := parseTracerouteReply(v4, pkt, localPort, ip)
		if !ok {
			return tracerouteEvent{}, false
		}
		// A port-unreachable straight from the target also proves arrival.
		if !reached {
			reached = peerIs(peer, ip)
		}
		return tracerouteEvent{ttl: ttl, idx: idx, peer: peerHost(peer), at: at, destReached: reached}, true
	}
	events := icmpReadSession(rawConn, deadline, 1500, tracerouteMaxHops*tracerouteProbesPerHop, parse)

	type hopState struct {
		addrs map[string]struct{}
		rtts  []int64
		got   int
	}
	hops := make([]hopState, tracerouteMaxHops)
	sendTimes := make([][tracerouteProbesPerHop]time.Time, tracerouteMaxHops)
	destReached := false
	lastTTL := 0

	record := func(ev tracerouteEvent) {
		h := &hops[ev.ttl-1]
		if h.addrs == nil {
			h.addrs = make(map[string]struct{})
		}
		h.addrs[ev.peer] = struct{}{}
		if st := sendTimes[ev.ttl-1][ev.idx]; !st.IsZero() {
			h.rtts = append(h.rtts, us(ev.at.Sub(st)))
		}
		h.got++
		if ev.destReached {
			destReached = true
		}
	}

ttlLoop:
	for ttl := 1; ttl <= tracerouteMaxHops; ttl++ {
		if ctx.Err() != nil {
			break
		}
		lastTTL = ttl
		if err := setTTL(ttl); err != nil {
			return fail(res, fmt.Errorf("set ttl %d: %w", ttl, err), pb.ProbeStatus_PROBE_STATUS_UNSPECIFIED)
		}
		for idx := 0; idx < tracerouteProbesPerHop; idx++ {
			dst := &net.UDPAddr{IP: ip, Port: tracerouteDstPort(ttl, idx)}
			sendTimes[ttl-1][idx] = time.Now()
			if _, err := udpConn.WriteTo([]byte("polarbeam"), dst); err != nil {
				return fail(res, err, pb.ProbeStatus_PROBE_STATUS_UNSPECIFIED)
			}
		}

		// Budget the wait so all remaining hops fit inside the run deadline.
		wait := budget(time.Until(deadline), tracerouteMaxHops-ttl+1, traceroutePerHopWait, 0)
		timer := time.NewTimer(wait)
		for hops[ttl-1].got < tracerouteProbesPerHop {
			select {
			case ev := <-events:
				record(ev)
			case <-timer.C:
				timer.Stop()
				if destReached {
					break ttlLoop
				}
				continue ttlLoop
			case <-ctx.Done():
				timer.Stop()
				break ttlLoop
			}
		}
		timer.Stop()
		if destReached {
			break
		}
	}

	pbHops := make([]*pb.Hop, lastTTL)
	for i := 0; i < lastTTL; i++ {
		addrs := make([]string, 0, len(hops[i].addrs))
		for a := range hops[i].addrs {
			addrs = append(addrs, a)
		}
		sort.Strings(addrs)
		pbHops[i] = &pb.Hop{Ttl: uint32(i + 1), Addrs: addrs, RttUs: hops[i].rtts}
	}
	res.Traceroute = &pb.TracerouteResult{
		Hops:        pbHops,
		DestReached: destReached,
		PathHash:    pathHash(pbHops),
	}

	if !destReached {
		res.Status = pb.ProbeStatus_PROBE_STATUS_TIMEOUT
		res.Error = fmt.Sprintf("destination %s not reached within %d hops", ip, lastTTL)
		return res
	}
	res.Received = 1
	res.Status = pb.ProbeStatus_PROBE_STATUS_OK
	return res
}

// tracerouteDstPort encodes (ttl, probe index) into the destination port so
// the quoted UDP header in an ICMP error identifies which probe elicited it.
func tracerouteDstPort(ttl, idx int) int {
	return tracerouteBasePort + (ttl-1)*tracerouteProbesPerHop + idx
}

// tracerouteFromPort is the inverse of tracerouteDstPort.
func tracerouteFromPort(port int) (ttl, idx int, ok bool) {
	off := port - tracerouteBasePort
	if off < 0 || off >= tracerouteMaxHops*tracerouteProbesPerHop {
		return 0, 0, false
	}
	return off/tracerouteProbesPerHop + 1, off % tracerouteProbesPerHop, true
}

// parseTracerouteReply validates an ICMP message against our probes: it must
// be time-exceeded or port-unreachable quoting a UDP packet from localPort to
// target, and its quoted destination port must decode to a (ttl, idx) we
// sent. reached reports port-unreachable (the probe hit the destination).
func parseTracerouteReply(v4 bool, b []byte, localPort int, target net.IP) (ttl, idx int, reached, ok bool) {
	proto := protoICMP
	if !v4 {
		proto = protoICMPv6
	}
	msg, err := icmp.ParseMessage(proto, b)
	if err != nil {
		return 0, 0, false, false
	}

	var quoted []byte
	switch body := msg.Body.(type) {
	case *icmp.TimeExceeded:
		quoted = body.Data
	case *icmp.DstUnreach:
		quoted = body.Data
		if v4 {
			reached = msg.Type == ipv4.ICMPTypeDestinationUnreachable && msg.Code == 3
		} else {
			reached = msg.Type == ipv6.ICMPTypeDestinationUnreachable && msg.Code == 4
		}
	default:
		return 0, 0, false, false
	}

	// Extract the quoted IP header + UDP header of our original probe. The
	// quoted protocol byte is deliberately not checked — see
	// TestParseTracerouteReplyIgnoresQuotedProto.
	_, quotedDst, udp, ok := quotedInner(v4, quoted)
	if !ok || !quotedDst.Equal(target) {
		return 0, 0, false, false
	}
	srcPort := int(binary.BigEndian.Uint16(udp[0:2]))
	dstPort := int(binary.BigEndian.Uint16(udp[2:4]))
	if srcPort != localPort {
		return 0, 0, false, false
	}
	ttl, idx, ok = tracerouteFromPort(dstPort)
	if !ok {
		return 0, 0, false, false
	}
	return ttl, idx, reached, true
}

func peerHost(addr net.Addr) string {
	switch a := addr.(type) {
	case *net.IPAddr:
		return a.IP.String()
	case *net.UDPAddr:
		return a.IP.String()
	default:
		return addr.String()
	}
}

func peerIs(addr net.Addr, ip net.IP) bool {
	switch a := addr.(type) {
	case *net.IPAddr:
		return a.IP.Equal(ip)
	case *net.UDPAddr:
		return a.IP.Equal(ip)
	default:
		return false
	}
}

// pathHash is sha256 over the hop address sequence, one line per hop: "*"
// for silent hops, otherwise the hop's unique addresses sorted and joined
// with ",". This must match the wire contract documented on
// TracerouteResult.path_hash — the server compares hashes byte-for-byte.
func pathHash(hops []*pb.Hop) []byte {
	lines := make([]string, len(hops))
	for i, h := range hops {
		addrs := h.GetAddrs()
		if len(addrs) == 0 {
			lines[i] = "*"
			continue
		}
		uniq := make([]string, 0, len(addrs))
		seen := make(map[string]struct{}, len(addrs))
		for _, a := range addrs {
			if _, dup := seen[a]; !dup {
				seen[a] = struct{}{}
				uniq = append(uniq, a)
			}
		}
		sort.Strings(uniq)
		lines[i] = strings.Join(uniq, ",")
	}
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return sum[:]
}
