package probes

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"math"
	"net"
	"sync"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
)

const (
	defaultTrainCount   = 10
	defaultTrainSpacing = 200 * time.Millisecond

	// IANA protocol numbers icmp.ParseMessage expects.
	protoICMP   = 1
	protoICMPv6 = 58
)

// ICMP sends trains of echo requests and reports loss, RTT statistics and
// RFC 3550 jitter. Jitter state is carried per probe series across runs, so
// the registry's single shared instance holds a keyed map.
type ICMP struct {
	mu    sync.Mutex
	state map[string]*jitterState // key: probe_id
}

// jitterState is the RFC 3550 interarrival-jitter accumulator for one series.
// The last RTT of a run seeds the fold of the next run's first RTT.
type jitterState struct {
	j       float64
	lastRTT int64
	primed  bool // lastRTT holds a real measurement
	hasJ    bool // at least one fold has happened; before that jitter is -1
}

func NewICMP() *ICMP { return &ICMP{state: make(map[string]*jitterState)} }

func (p *ICMP) Run(ctx context.Context, spec *pb.ProbeSpec) *pb.ProbeResult {
	start := time.Now()
	res := newResult(spec, start)

	count := int(spec.GetTrainCount())
	if count <= 0 {
		count = defaultTrainCount
	}
	spacing := spec.GetTrainSpacing().AsDuration()
	if spacing <= 0 {
		spacing = defaultTrainSpacing
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = start.Add(5 * time.Second)
	}

	ip, err := resolveIP(ctx, spec.GetTarget().GetAddress())
	if err != nil {
		return fail(res, err, pb.ProbeStatus_PROBE_STATUS_UNSPECIFIED)
	}

	conn, mode, err := openICMP(ip)
	if err != nil {
		res.Status = pb.ProbeStatus_PROBE_STATUS_ERROR
		res.Error = err.Error()
		return res
	}
	defer conn.Close()
	conn.SetReadDeadline(deadline)

	// Random per-run token: under datagram ICMP the kernel rewrites the echo
	// ID to the socket's local port, so replies are matched by seq + token,
	// never by ID. In raw mode the socket sees every echo reply on the host,
	// so the (random) ID is checked as well.
	var token [8]byte
	if _, err := rand.Read(token[:]); err != nil {
		res.Status = pb.ProbeStatus_PROBE_STATUS_ERROR
		res.Error = fmt.Sprintf("random token: %v", err)
		return res
	}
	id := int(binary.BigEndian.Uint16(token[:2]))

	type reply struct {
		seq int
		at  time.Time
	}
	replies := make(chan reply, count)
	go func() {
		buf := make([]byte, 1500)
		for {
			n, _, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			msg, err := icmp.ParseMessage(mode.proto, buf[:n])
			if err != nil || msg.Type != mode.echoReply {
				continue
			}
			echo, ok := msg.Body.(*icmp.Echo)
			if !ok || string(echo.Data) != string(token[:]) {
				continue
			}
			if mode.raw && echo.ID != id {
				continue
			}
			if echo.Seq < 1 || echo.Seq > count {
				continue
			}
			select { // never block: the run may already be over
			case replies <- reply{seq: echo.Seq, at: time.Now()}:
			default:
			}
		}
	}()

	sendTimes := make([]time.Time, count+1) // 1-based seq
	sent := 0
	for seq := 1; seq <= count; seq++ {
		if seq > 1 {
			select {
			case <-time.After(spacing):
			case <-ctx.Done():
			}
			if ctx.Err() != nil {
				break
			}
		}
		wm := icmp.Message{
			Type: mode.echoRequest,
			Body: &icmp.Echo{ID: id, Seq: seq, Data: token[:]},
		}
		wb, err := wm.Marshal(nil)
		if err != nil {
			res.Status = pb.ProbeStatus_PROBE_STATUS_ERROR
			res.Error = fmt.Sprintf("marshal echo: %v", err)
			return res
		}
		sendTimes[seq] = time.Now()
		if _, err := conn.WriteTo(wb, mode.dst); err != nil {
			return fail(res, err, pb.ProbeStatus_PROBE_STATUS_UNSPECIFIED)
		}
		sent++
	}

	// Collect replies (in seq order for the jitter fold) until the train is
	// fully answered or the run deadline expires.
	rttBySeq := make(map[int]int64, sent)
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
collect:
	for len(rttBySeq) < sent {
		select {
		case r := <-replies:
			if _, dup := rttBySeq[r.seq]; dup || sendTimes[r.seq].IsZero() {
				continue
			}
			rttBySeq[r.seq] = us(r.at.Sub(sendTimes[r.seq]))
		case <-timer.C:
			break collect
		case <-ctx.Done():
			break collect
		}
	}

	rtts := make([]int64, 0, len(rttBySeq))
	for seq := 1; seq <= count; seq++ {
		if rtt, ok := rttBySeq[seq]; ok {
			rtts = append(rtts, rtt)
		}
	}

	res.Sent = uint32(sent)
	res.Received = uint32(len(rtts))
	res.JitterUs = p.foldJitter(spec.GetProbeId(), rtts)
	if len(rtts) == 0 {
		res.Status = pb.ProbeStatus_PROBE_STATUS_TIMEOUT
		res.Error = fmt.Sprintf("no echo replies from %s", ip)
		return res
	}
	min, avg, max, stddev := rttStats(rtts)
	res.Rtt = &pb.RttStats{MinUs: min, AvgUs: avg, MaxUs: max, StddevUs: stddev}
	res.Status = pb.ProbeStatus_PROBE_STATUS_OK
	return res
}

// foldJitter advances the series' RFC 3550 accumulator with this run's RTTs
// (seq order) and returns the jitter to report: -1 until two consecutive
// RTTs have ever been observed for the series.
func (p *ICMP) foldJitter(probeID string, rtts []int64) int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.state[probeID]
	if !ok {
		st = &jitterState{}
		p.state[probeID] = st
	}
	for _, rtt := range rtts {
		if st.primed {
			d := float64(rtt - st.lastRTT)
			st.j += (math.Abs(d) - st.j) / 16
			st.hasJ = true
		}
		st.lastRTT = rtt
		st.primed = true
	}
	if !st.hasJ {
		return -1
	}
	return int64(st.j + 0.5)
}

// Retire drops the jitter accumulator for a retired series (probes.Retirer).
// Called by the scheduler after the series' worker has exited, so it cannot
// race that worker's own foldJitter.
func (p *ICMP) Retire(probeID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.state, probeID)
}

// rttStats returns min/avg/max/population-stddev of rtts in microseconds.
// rtts must be non-empty.
func rttStats(rtts []int64) (min, avg, max, stddev int64) {
	min, max = rtts[0], rtts[0]
	var sum int64
	for _, r := range rtts {
		if r < min {
			min = r
		}
		if r > max {
			max = r
		}
		sum += r
	}
	mean := float64(sum) / float64(len(rtts))
	var sq float64
	for _, r := range rtts {
		d := float64(r) - mean
		sq += d * d
	}
	return min, int64(mean + 0.5), max, int64(math.Sqrt(sq/float64(len(rtts))) + 0.5)
}

// resolveIP resolves a hostname or literal to a single IP, preferring IPv4.
func resolveIP(ctx context.Context, address string) (net.IP, error) {
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", address)
	if err != nil {
		return nil, err
	}
	for _, ip := range ips {
		if ip.To4() != nil {
			return ip, nil
		}
	}
	if len(ips) == 0 {
		return nil, &net.DNSError{Err: "no addresses", Name: address}
	}
	return ips[0], nil
}

// icmpMode captures everything address-family- and socket-type-specific.
type icmpMode struct {
	proto       int
	echoRequest icmp.Type
	echoReply   icmp.Type
	dst         net.Addr
	raw         bool
}

// openICMP opens an ICMP socket for ip: unprivileged datagram first (works
// when net.ipv4.ping_group_range covers the process group), raw as fallback
// (needs CAP_NET_RAW). Both are tried every run so a capability change never
// needs an agent restart.
func openICMP(ip net.IP) (*icmp.PacketConn, icmpMode, error) {
	v4 := ip.To4() != nil
	mode := icmpMode{proto: protoICMP, echoRequest: ipv4.ICMPTypeEcho, echoReply: ipv4.ICMPTypeEchoReply}
	dgramNet, rawNet := "udp4", "ip4:icmp"
	if !v4 {
		mode = icmpMode{proto: protoICMPv6, echoRequest: ipv6.ICMPTypeEchoRequest, echoReply: ipv6.ICMPTypeEchoReply}
		dgramNet, rawNet = "udp6", "ip6:ipv6-icmp"
	}

	conn, dgramErr := icmp.ListenPacket(dgramNet, "")
	if dgramErr == nil {
		mode.dst = &net.UDPAddr{IP: ip}
		return conn, mode, nil
	}
	conn, rawErr := icmp.ListenPacket(rawNet, "")
	if rawErr == nil {
		mode.dst = &net.IPAddr{IP: ip}
		mode.raw = true
		return conn, mode, nil
	}
	return nil, mode, fmt.Errorf(
		"no ICMP socket available: datagram (%v; check net.ipv4.ping_group_range) and raw (%v; needs CAP_NET_RAW)",
		dgramErr, rawErr)
}
