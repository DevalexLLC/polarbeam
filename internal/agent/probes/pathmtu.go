package probes

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
	"net"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
)

const (
	// All path MTU sizes are IP PACKET bytes INCLUDING the IP header, so
	// they compare directly to interface and link MTUs (the wire contract
	// on PathMtuResult). Keep defaults/bounds in lockstep with
	// internal/server/probeadmin.
	defaultMTUMin = 1280 // IPv6 minimum link MTU — a safe floor everywhere
	defaultMTUMax = 1500 // classic Ethernet
	mtuFloor      = 68   // IPv4 minimum MTU (RFC 791)
	mtuCeil       = 9216 // jumbo frames
	ipv6MinMTU    = 1280

	ipv4HeaderLen     = 20
	ipv6HeaderLen     = 40
	icmpEchoHeaderLen = 8

	// A size is declared silent only after this many straight unanswered
	// sends, so one lost packet never masquerades as the MTU ceiling.
	pmtuRetriesPerSize = 3
	pmtuPerProbeWait   = time.Second
	// Floor on the per-attempt wait even when the deadline budget is
	// tight: a too-short wait misreads a slow reply as silence and
	// corrupts the search bracket. Honest non-convergence is preferred.
	pmtuMinWait = 200 * time.Millisecond
)

// PathMTU determines the largest IP packet a path carries without
// fragmentation: ICMP echo requests padded to candidate sizes with DF set
// (Linux PMTU-probe socket options), bounded by a binary search between
// mtu.min and mtu.max. A valid ICMPv4 Fragmentation Needed or ICMPv6
// Packet Too Big bounds the search and reports the advertised next-hop
// MTU; repeated silence above a proven-good size is a suspected PMTU
// black hole. Like traceroute, the prober must observe ICMP errors
// elicited by its own packets, so CAP_NET_RAW is a hard requirement; its
// absence is an ERROR result every cadence, never a silent skip.
type PathMTU struct{}

// pmtuEvent is one matched reply from the reader: an echo reply or a
// Fragmentation Needed / Packet Too Big, with the advertised MTU.
type pmtuEvent struct {
	kind replyKind
	seq  int
	mtu  int
	at   time.Time
}

func (PathMTU) Run(ctx context.Context, spec *pb.ProbeSpec) *pb.ProbeResult {
	start := time.Now()
	res := newResult(spec, start)
	deadline := runDeadline(ctx, start, 5*time.Second)

	minSize, maxSize, family, err := pmtuParams(spec.GetParams())
	if err != nil {
		// The server validates params before they reach an agent; this
		// guards against skew between server and agent versions.
		res.Status = pb.ProbeStatus_PROBE_STATUS_ERROR
		res.Error = err.Error()
		return res
	}

	ip, err := resolveIPFamily(ctx, spec.GetTarget().GetAddress(), family)
	if err != nil {
		return fail(res, err, pb.ProbeStatus_PROBE_STATUS_UNSPECIFIED)
	}
	v6 := ip.To4() == nil

	rawNet, echoRequest, ipHdrLen := "ip4:icmp", icmp.Type(ipv4.ICMPTypeEcho), ipv4HeaderLen
	if v6 {
		rawNet, echoRequest, ipHdrLen = "ip6:ipv6-icmp", ipv6.ICMPTypeEchoRequest, ipv6HeaderLen
	}
	conn, err := net.ListenPacket(rawNet, "")
	if err != nil {
		res.Status = pb.ProbeStatus_PROBE_STATUS_ERROR
		res.Error = fmt.Sprintf("path MTU probing requires CAP_NET_RAW (raw ICMP socket): %v", err)
		return res
	}
	defer conn.Close()
	ipConn, isIPConn := conn.(*net.IPConn)
	if !isIPConn {
		res.Status = pb.ProbeStatus_PROBE_STATUS_ERROR
		res.Error = fmt.Sprintf("unexpected raw socket type %T", conn)
		return res
	}
	if err := setDontFragment(ipConn, v6); err != nil {
		res.Status = pb.ProbeStatus_PROBE_STATUS_ERROR
		res.Error = fmt.Sprintf("cannot enable path MTU probe mode: %v", err)
		return res
	}
	// Random per-run token: the raw socket sees every ICMP message on the
	// host, so echo replies are matched by ID + token prefix. ICMP errors
	// quote only the first 8 bytes past the IP header — exactly the echo
	// header — so there they are matched by ID + seq alone.
	token, id, err := echoToken()
	if err != nil {
		res.Status = pb.ProbeStatus_PROBE_STATUS_ERROR
		res.Error = err.Error()
		return res
	}

	parse := func(pkt []byte, _ net.Addr, at time.Time) (pmtuEvent, bool) {
		kind, seq, mtu := parsePMTUReply(v6, pkt, id, token[:], ip)
		if kind == replyNone {
			return pmtuEvent{}, false
		}
		return pmtuEvent{kind: kind, seq: seq, mtu: mtu, at: at}, true
	}
	events := icmpReadSession(ipConn, deadline, 65536, 64, parse)

	search := newPMTUSearch(minSize, maxSize, v6)
	folder := &pmtuFolder{
		search:    search,
		sentAt:    make(map[int]sentProbe),
		rttBySize: make(map[int]int64),
	}
	seq := 0

sizeLoop:
	for {
		size, more := search.next()
		if !more {
			break
		}
		verdict, adv := verdictSilent, 0
	attempts:
		for range pmtuRetriesPerSize {
			remaining := time.Until(deadline)
			if ctx.Err() != nil || remaining <= 0 {
				break sizeLoop
			}
			seq++
			payload := make([]byte, size-ipHdrLen-icmpEchoHeaderLen)
			copy(payload, token[:])
			wm := icmp.Message{
				Type: echoRequest,
				Body: &icmp.Echo{ID: id, Seq: seq, Data: payload},
			}
			wb, err := wm.Marshal(nil)
			if err != nil {
				res.Status = pb.ProbeStatus_PROBE_STATUS_ERROR
				res.Error = fmt.Sprintf("marshal echo: %v", err)
				return res
			}
			at := time.Now()
			if _, err := ipConn.WriteTo(wb, &net.IPAddr{IP: ip}); err != nil {
				if errors.Is(err, syscall.EMSGSIZE) {
					verdict = verdictLocal
					break attempts
				}
				return fail(res, err, pb.ProbeStatus_PROBE_STATUS_UNSPECIFIED)
			}
			folder.sentAt[seq] = sentProbe{size: size, at: at}

			// Budget the wait so the remaining search fits inside the run
			// deadline; the floor keeps a tight budget from misreading a
			// slow reply as silence.
			wait := budget(remaining, pmtuRetriesPerSize*search.sizesLeft(), pmtuPerProbeWait, pmtuMinWait)
			timer := time.NewTimer(wait)
			for {
				select {
				case ev := <-events:
					if r, v, a := folder.fold(ev, size); r {
						timer.Stop()
						verdict, adv = v, a
						break attempts
					}
				case <-timer.C:
					continue attempts
				case <-ctx.Done():
					timer.Stop()
					break sizeLoop
				}
			}
		}
		search.record(size, verdict, adv)
	}

	return pmtuFinalize(res, search, folder.rttBySize, v6, minSize, ip)
}

// sentProbe is one outstanding echo: the size it tested and when it left.
type sentProbe struct {
	size int
	at   time.Time
}

// pmtuFolder applies reply events to the search. Events for the size
// currently under test resolve it (fold returns true); late events for
// earlier sizes are still evidence and go straight into the search.
type pmtuFolder struct {
	search    *pmtuSearch
	sentAt    map[int]sentProbe
	rttBySize map[int]int64 // best RTT per size, for the result's RttUs
}

func (f *pmtuFolder) fold(ev pmtuEvent, curSize int) (resolved bool, v sizeVerdict, adv int) {
	sp, known := f.sentAt[ev.seq]
	if !known {
		return false, 0, 0
	}
	switch ev.kind {
	case replyEcho:
		rtt := us(ev.at.Sub(sp.at))
		if old, dup := f.rttBySize[sp.size]; !dup || rtt < old {
			f.rttBySize[sp.size] = rtt
		}
		if sp.size == curSize {
			return true, verdictOK, 0
		}
		f.search.record(sp.size, verdictOK, 0)
	case replyTooBig:
		if sp.size == curSize {
			return true, verdictTooBig, ev.mtu
		}
		f.search.record(sp.size, verdictTooBig, ev.mtu)
	}
	return false, 0, 0
}

// pmtuFinalize fills res from the finished (or abandoned) search: the
// result payload and the status classification.
func pmtuFinalize(res *pb.ProbeResult, search *pmtuSearch, rttBySize map[int]int64, v6 bool, minSize int, ip net.IP) *pb.ProbeResult {
	o := search.outcome()
	ipVer := uint32(4)
	if v6 {
		ipVer = 6
	}
	rtt := int64(-1)
	if r, measured := rttBySize[o.largestOK]; measured {
		rtt = r
	}
	res.PathMtu = &pb.PathMtuResult{
		LargestOkBytes:      uint32(o.largestOK),
		SmallestFailedBytes: uint32(o.smallestFailed),
		NextHopMtuBytes:     uint32(o.nextHopMTU),
		IpVersion:           ipVer,
		BlackHoleSuspected:  o.blackHole,
		RttUs:               rtt,
		LocalConstraint:     o.localConstraint,
	}
	switch {
	case !search.done():
		res.Status = pb.ProbeStatus_PROBE_STATUS_TIMEOUT
		res.Error = "path MTU search did not converge within the run timeout"
	case o.localConstraint && o.largestOK == 0:
		res.Status = pb.ProbeStatus_PROBE_STATUS_ERROR
		res.Error = fmt.Sprintf("local interface cannot send %d-byte packets (mtu.min)", minSize)
	case o.blackHole:
		res.Status = pb.ProbeStatus_PROBE_STATUS_TIMEOUT
		res.Error = fmt.Sprintf(
			"suspected path MTU black hole: %d-byte packets pass, larger ones vanish without any ICMP error",
			o.largestOK)
	case o.largestOK == 0 && o.cause == causeTooBig:
		res.Status = pb.ProbeStatus_PROBE_STATUS_ERROR
		res.Error = fmt.Sprintf("path cannot carry %d-byte packets (mtu.min): ICMP reports a smaller MTU", minSize)
	case o.largestOK == 0:
		res.Status = pb.ProbeStatus_PROBE_STATUS_TIMEOUT
		res.Error = fmt.Sprintf("no echo replies from %s at any tested size", ip)
	default:
		res.Status = pb.ProbeStatus_PROBE_STATUS_OK
	}
	return res
}

// pmtuParams applies defaults and re-validates type-specific parameters.
func pmtuParams(params map[string]string) (minSize, maxSize int, family string, err error) {
	minSize, maxSize = defaultMTUMin, defaultMTUMax
	if v, set := params["mtu.min"]; set {
		if minSize, err = strconv.Atoi(v); err != nil {
			return 0, 0, "", fmt.Errorf("mtu.min: %v", err)
		}
	}
	if v, set := params["mtu.max"]; set {
		if maxSize, err = strconv.Atoi(v); err != nil {
			return 0, 0, "", fmt.Errorf("mtu.max: %v", err)
		}
	}
	if minSize < mtuFloor || minSize > mtuCeil || maxSize < mtuFloor || maxSize > mtuCeil {
		return 0, 0, "", fmt.Errorf("mtu.min/mtu.max must be within %d-%d bytes", mtuFloor, mtuCeil)
	}
	if minSize >= maxSize {
		return 0, 0, "", fmt.Errorf("mtu.min (%d) must be less than mtu.max (%d)", minSize, maxSize)
	}
	family = params["mtu.family"]
	if family != "" && family != "4" && family != "6" {
		return 0, 0, "", fmt.Errorf("mtu.family must be 4 or 6")
	}
	return minSize, maxSize, family, nil
}

// resolveIPFamily resolves like resolveIP (IPv4 preferred) when family is
// empty, otherwise restricts resolution to the requested IP version.
func resolveIPFamily(ctx context.Context, address, family string) (net.IP, error) {
	if family == "" {
		return resolveIP(ctx, address)
	}
	network := "ip4"
	if family == "6" {
		network = "ip6"
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, network, address)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, &net.DNSError{Err: "no IPv" + family + " addresses", Name: address}
	}
	return ips[0], nil
}

type sizeVerdict int

const (
	verdictOK     sizeVerdict = iota // echo reply at this size
	verdictTooBig                    // valid Fragmentation Needed / Packet Too Big
	verdictSilent                    // all retries at this size unanswered
	verdictLocal                     // EMSGSIZE on send (local interface limit)
)

// hiCause records what kind of failure set the current upper bound.
type hiCause int

const (
	causeNone hiCause = iota
	causeTooBig
	causeSilent
	causeLocal
)

// pmtuSearch is the socket-free bounded search over packet sizes: probe
// max first (a clean path converges in one round trip), then min, then
// bisect, testing a plausible ICMP-advertised MTU directly when one
// arrives. At most ~log2(max-min)+2 sizes are ever tested — never a scan.
type pmtuSearch struct {
	min, max int
	v6       bool

	lo     int // largest size proven delivered; 0 until one is
	hi     int // smallest size proven failed; max+1 until one is
	cause  hiCause
	tested map[int]bool

	nextHopMTU   int  // smallest plausible ICMP-advertised MTU seen
	override     int  // advertised size to test next; 0 = none
	advConfirmed bool // the advertised MTU itself tested OK
}

func newPMTUSearch(min, max int, v6 bool) *pmtuSearch {
	return &pmtuSearch{min: min, max: max, v6: v6, hi: max + 1, tested: make(map[int]bool)}
}

// done reports whether the bracket is closed: max passed, min failed, the
// bounds meet, or the ICMP-advertised MTU was verified end to end (real
// PMTUD trusts the advertised value; bisecting the residual gap would be
// pure extra traffic for a rare second bottleneck).
func (s *pmtuSearch) done() bool {
	return s.lo >= s.max || s.hi <= s.min || s.hi-s.lo <= 1 || s.advConfirmed
}

// next returns the next size to test; more=false when the search is done
// or nothing new can be learned.
func (s *pmtuSearch) next() (size int, more bool) {
	if s.done() {
		return 0, false
	}
	switch {
	case !s.tested[s.max]:
		return s.max, true
	case s.override > s.lo && s.override < s.hi && !s.tested[s.override]:
		return s.override, true
	case s.lo == 0 && !s.tested[s.min]:
		return s.min, true
	}
	mid := (s.lo + s.hi) / 2
	if s.tested[mid] || mid <= s.lo || mid >= s.hi {
		return 0, false
	}
	return mid, true
}

// record folds one verdict. Monotonicity is guarded: evidence can tighten
// the bracket but never widen it, so late or contradictory replies are
// safe to fold in any order.
func (s *pmtuSearch) record(size int, v sizeVerdict, advMTU int) {
	s.tested[size] = true
	switch v {
	case verdictOK:
		if size > s.lo && size < s.hi {
			s.lo = size
			if s.cause == causeTooBig && size == s.nextHopMTU {
				s.advConfirmed = true
			}
		}
	case verdictTooBig:
		if size < s.hi {
			s.hi = size
			s.cause = causeTooBig
		}
		floor := mtuFloor
		if s.v6 {
			floor = ipv6MinMTU
		}
		if advMTU >= floor && advMTU < size && (s.nextHopMTU == 0 || advMTU < s.nextHopMTU) {
			s.nextHopMTU = advMTU
			if advMTU >= s.min {
				s.override = advMTU
			}
		}
	case verdictSilent:
		if size < s.hi {
			s.hi = size
			s.cause = causeSilent
		}
	case verdictLocal:
		if size < s.hi {
			s.hi = size
			s.cause = causeLocal
		}
	}
}

// sizesLeft is a small upper estimate of how many sizes remain, used to
// budget per-attempt waits against the run deadline.
func (s *pmtuSearch) sizesLeft() int {
	return bits.Len(uint(s.hi-s.lo)) + 1
}

type pmtuOutcome struct {
	largestOK, smallestFailed, nextHopMTU int
	cause                                 hiCause
	blackHole, localConstraint            bool
}

func (s *pmtuSearch) outcome() pmtuOutcome {
	o := pmtuOutcome{largestOK: s.lo, nextHopMTU: s.nextHopMTU, cause: s.cause}
	if s.hi <= s.max {
		o.smallestFailed = s.hi
	}
	o.blackHole = s.done() && s.lo > 0 && s.cause == causeSilent
	o.localConstraint = s.cause == causeLocal
	return o
}

type replyKind int

const (
	replyNone replyKind = iota
	replyEcho
	replyTooBig
)

// parsePMTUReply classifies one raw ICMP read against our probes: an echo
// reply (matched by ID + token prefix), or a Fragmentation Needed /
// Packet Too Big whose quoted packet provably wraps one of our echoes
// (matched by ID + seq — RFC 792 quotes only the first 8 bytes past the
// IP header, so the token is not available there). mtu is the advertised
// next-hop MTU, 0 when absent. Anything short, mismatched, or otherwise
// not provably ours is replyNone.
func parsePMTUReply(v6 bool, b []byte, id int, token []byte, target net.IP) (kind replyKind, seq, mtu int) {
	proto := protoICMP
	if v6 {
		proto = protoICMPv6
	}
	msg, err := icmp.ParseMessage(proto, b)
	if err != nil {
		return replyNone, 0, 0
	}

	if (!v6 && msg.Type == ipv4.ICMPTypeEchoReply) || (v6 && msg.Type == ipv6.ICMPTypeEchoReply) {
		echo, isEcho := msg.Body.(*icmp.Echo)
		if !isEcho || echo.ID != id || len(echo.Data) < len(token) || !bytes.Equal(echo.Data[:len(token)], token) {
			return replyNone, 0, 0
		}
		return replyEcho, echo.Seq, 0
	}

	if !v6 {
		// Fragmentation Needed is destination-unreachable code 4, and only
		// code 4 — other unreachable codes say nothing about the MTU.
		if msg.Type != ipv4.ICMPTypeDestinationUnreachable || msg.Code != 4 {
			return replyNone, 0, 0
		}
		body, isUnreach := msg.Body.(*icmp.DstUnreach)
		if !isUnreach || len(b) < 8 {
			return replyNone, 0, 0
		}
		// x/net's DstUnreach drops the second header word, which for code 4
		// carries the next-hop MTU (RFC 1191) — read it from the raw bytes.
		mtu = int(binary.BigEndian.Uint16(b[6:8]))
		qproto, quotedDst, e, ok := quotedInner(true, body.Data)
		if !ok || qproto != protoICMP || !quotedDst.Equal(target.To4()) {
			return replyNone, 0, 0
		}
		if e[0] != 8 /* echo request */ || int(binary.BigEndian.Uint16(e[4:6])) != id {
			return replyNone, 0, 0
		}
		return replyTooBig, int(binary.BigEndian.Uint16(e[6:8])), mtu
	}

	if msg.Type != ipv6.ICMPTypePacketTooBig {
		return replyNone, 0, 0
	}
	body, isPTB := msg.Body.(*icmp.PacketTooBig)
	if !isPTB {
		return replyNone, 0, 0
	}
	mtu = body.MTU
	qproto, quotedDst, e, ok := quotedInner(false, body.Data)
	if !ok || qproto != protoICMPv6 || !quotedDst.Equal(target) {
		return replyNone, 0, 0
	}
	if e[0] != 128 /* echo request */ || int(binary.BigEndian.Uint16(e[4:6])) != id {
		return replyNone, 0, 0
	}
	return replyTooBig, int(binary.BigEndian.Uint16(e[6:8])), mtu
}
