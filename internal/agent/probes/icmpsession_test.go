package probes

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// fakePacketConn scripts ReadFrom: it serves its frames in order, then
// closes done and returns readErr forever after. The reader goroutine is
// the only ReadFrom caller and tests inspect state only after <-done, so
// no locking is needed.
type fakePacketConn struct {
	frames      []fakeFrame
	readErr     error
	deadlines   []time.Time
	deadlineErr error
	done        chan struct{}
	closed      bool
}

type fakeFrame struct {
	payload []byte
	peer    net.Addr
}

func newFakePacketConn(frames ...fakeFrame) *fakePacketConn {
	return &fakePacketConn{frames: frames, readErr: io.EOF, done: make(chan struct{})}
}

func (f *fakePacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	if len(f.frames) == 0 {
		if !f.closed {
			f.closed = true
			close(f.done)
		}
		return 0, nil, f.readErr
	}
	fr := f.frames[0]
	f.frames = f.frames[1:]
	return copy(b, fr.payload), fr.peer, nil
}

func (f *fakePacketConn) SetReadDeadline(t time.Time) error {
	f.deadlines = append(f.deadlines, t)
	return f.deadlineErr
}

func waitDone(t *testing.T, f *fakePacketConn) {
	t.Helper()
	select {
	case <-f.done:
	case <-time.After(5 * time.Second):
		t.Fatal("reader goroutine did not drain the fake conn")
	}
}

type fakeEvent struct {
	payload string
	peer    net.Addr
	at      time.Time
}

func fakeParse(pkt []byte, peer net.Addr, at time.Time) (fakeEvent, bool) {
	if string(pkt) == "drop" {
		return fakeEvent{}, false
	}
	return fakeEvent{payload: string(pkt), peer: peer, at: at}, true
}

func TestICMPReadSessionDelivers(t *testing.T) {
	peer := &net.UDPAddr{IP: net.ParseIP("192.0.2.1"), Port: 9}
	f := newFakePacketConn(
		fakeFrame{payload: []byte("aa"), peer: peer},
		fakeFrame{payload: []byte("drop"), peer: peer},
		fakeFrame{payload: []byte("bb"), peer: peer},
	)
	events := icmpReadSession(f, time.Now().Add(time.Minute), 1500, 4, fakeParse)
	waitDone(t, f)

	if got := len(events); got != 2 {
		t.Fatalf("delivered %d events, want 2 (unparseable frame must be dropped)", got)
	}
	first, second := <-events, <-events
	if first.payload != "aa" || second.payload != "bb" {
		t.Errorf("events out of order: %q, %q", first.payload, second.payload)
	}
	if first.peer != peer || first.at.IsZero() {
		t.Errorf("event lost the peer or timestamp: %+v", first)
	}
}

// The read buffer bounds what parse sees, exactly like a real socket read.
func TestICMPReadSessionBufferBounds(t *testing.T) {
	f := newFakePacketConn(fakeFrame{payload: []byte("0123456789")})
	events := icmpReadSession(f, time.Now().Add(time.Minute), 4, 1, fakeParse)
	waitDone(t, f)
	if ev := <-events; ev.payload != "0123" {
		t.Errorf("parse saw %q, want the 4-byte read %q", ev.payload, "0123")
	}
}

// The reader must never block on a full channel: with nobody draining, it
// still consumes every frame and exits, and the channel holds the events
// that fit.
func TestICMPReadSessionNeverBlocks(t *testing.T) {
	f := newFakePacketConn(
		fakeFrame{payload: []byte("one")},
		fakeFrame{payload: []byte("two")},
		fakeFrame{payload: []byte("three")},
	)
	events := icmpReadSession(f, time.Now().Add(time.Minute), 1500, 1, fakeParse)
	waitDone(t, f)
	if got := len(events); got != 1 {
		t.Fatalf("channel holds %d events, want 1 (overflow must be dropped, not queued)", got)
	}
	if ev := <-events; ev.payload != "one" {
		t.Errorf("kept event = %q, want the first frame %q", ev.payload, "one")
	}
}

func TestICMPReadSessionDeadline(t *testing.T) {
	deadline := time.Now().Add(42 * time.Second)
	f := newFakePacketConn(fakeFrame{payload: []byte("aa")})
	// A SetReadDeadline error is discarded and the session runs anyway,
	// as all three probers have always behaved.
	f.deadlineErr = errors.New("deadline unsupported")
	events := icmpReadSession(f, deadline, 1500, 1, fakeParse)
	if len(f.deadlines) != 1 || !f.deadlines[0].Equal(deadline) {
		t.Errorf("SetReadDeadline calls = %v, want exactly [%v]", f.deadlines, deadline)
	}
	waitDone(t, f)
	if ev := <-events; ev.payload != "aa" {
		t.Errorf("session did not deliver despite the discarded deadline error: %+v", ev)
	}
}

// budget must be byte-for-byte equivalent to the two formulas it replaced:
// traceroute's (no floor, no clamp — a negative remaining passes through)
// and pathmtu's (floor, then clamp to remaining, in that order).
func TestBudgetMatchesLegacyFormulas(t *testing.T) {
	legacyTraceroute := func(remaining time.Duration, rounds int) time.Duration {
		wait := traceroutePerHopWait
		if per := remaining / time.Duration(rounds); per < wait {
			wait = per
		}
		return wait
	}
	legacyPMTU := func(remaining time.Duration, rounds int) time.Duration {
		wait := pmtuPerProbeWait
		if per := remaining / time.Duration(rounds); per < wait {
			wait = per
		}
		if wait < pmtuMinWait {
			wait = pmtuMinWait
		}
		if wait > remaining {
			wait = remaining
		}
		return wait
	}

	remainings := []time.Duration{
		-10 * time.Millisecond, 0, 50 * time.Millisecond, 100 * time.Millisecond,
		150 * time.Millisecond, 999 * time.Millisecond, time.Second,
		1500 * time.Millisecond, 5 * time.Second, 30 * time.Second,
	}
	rounds := []int{1, 2, 3, 15, 30, 90}
	for _, rem := range remainings {
		for _, n := range rounds {
			if got, want := budget(rem, n, traceroutePerHopWait, 0), legacyTraceroute(rem, n); got != want {
				t.Errorf("budget(%v, %d, %v, 0) = %v, want traceroute legacy %v", rem, n, traceroutePerHopWait, got, want)
			}
			if got, want := budget(rem, n, pmtuPerProbeWait, pmtuMinWait), legacyPMTU(rem, n); got != want {
				t.Errorf("budget(%v, %d, %v, %v) = %v, want pathmtu legacy %v", rem, n, pmtuPerProbeWait, pmtuMinWait, got, want)
			}
		}
	}
}

func TestQuotedInnerV4(t *testing.T) {
	dst := net.ParseIP("192.0.2.7")

	q := quotedV4(11, 22, dst)
	q[9] = 17 // UDP
	proto, gotDst, inner, ok := quotedInner(true, q)
	if !ok || proto != 17 || !gotDst.Equal(dst) || len(inner) != 8 {
		t.Fatalf("well-formed quote = (%d, %v, %d bytes, %v)", proto, gotDst, len(inner), ok)
	}
	if binary.BigEndian.Uint16(inner[0:2]) != 11 || binary.BigEndian.Uint16(inner[2:4]) != 22 {
		t.Errorf("inner bytes are not the quoted transport header: %x", inner)
	}

	// IP options shift the inner header: IHL 6 puts it at offset 24.
	opts := make([]byte, 32)
	opts[0] = 0x46
	opts[9] = 17
	copy(opts[16:20], dst.To4())
	binary.BigEndian.PutUint16(opts[24:26], 33)
	_, _, inner, ok = quotedInner(true, opts)
	if !ok || binary.BigEndian.Uint16(inner[0:2]) != 33 {
		t.Errorf("IHL 6 quote: inner = %x, ok = %v; want inner at offset 24", inner, ok)
	}

	for name, bad := range map[string][]byte{
		"shorter than an IP header": q[:19],
		"header length below 20":    append([]byte{0x44}, q[1:]...),
		"truncated inner header":    q[:25],
		"options overrun":           opts[:31],
	} {
		if _, _, _, ok := quotedInner(true, bad); ok {
			t.Errorf("%s must not parse", name)
		}
	}
}

func TestQuotedInnerV6(t *testing.T) {
	dst := net.ParseIP("2001:db8::7")
	q := make([]byte, 48)
	q[6] = 58 // ICMPv6
	copy(q[24:40], dst)
	binary.BigEndian.PutUint16(q[40:42], 44)

	proto, gotDst, inner, ok := quotedInner(false, q)
	if !ok || proto != 58 || !gotDst.Equal(dst) || binary.BigEndian.Uint16(inner[0:2]) != 44 {
		t.Fatalf("well-formed v6 quote = (%d, %v, %x, %v)", proto, gotDst, inner, ok)
	}
	if _, _, _, ok := quotedInner(false, q[:47]); ok {
		t.Error("a quote shorter than header+8 must not parse")
	}
}

func TestEchoToken(t *testing.T) {
	token, id, err := echoToken()
	if err != nil {
		t.Fatalf("echoToken: %v", err)
	}
	if want := int(binary.BigEndian.Uint16(token[:2])); id != want {
		t.Errorf("id = %d, want the token's first two bytes %d", id, want)
	}
}

func TestRunDeadline(t *testing.T) {
	start := time.Now()
	if d := runDeadline(context.Background(), start, 5*time.Second); !d.Equal(start.Add(5 * time.Second)) {
		t.Errorf("fallback deadline = %v, want start+5s", d)
	}
	want := start.Add(time.Minute)
	ctx, cancel := context.WithDeadline(context.Background(), want)
	defer cancel()
	if d := runDeadline(ctx, start, 5*time.Second); !d.Equal(want) {
		t.Errorf("context deadline = %v, want %v", d, want)
	}
}

func TestICMPEchoParser(t *testing.T) {
	var token [8]byte
	copy(token[:], "tokentok")
	id := int(binary.BigEndian.Uint16(token[:2]))
	mode := icmpMode{proto: protoICMP, echoReply: ipv4.ICMPTypeEchoReply, raw: true}

	reply := func(t *testing.T, msgID, seq int, data []byte) []byte {
		t.Helper()
		b, err := (&icmp.Message{
			Type: ipv4.ICMPTypeEchoReply,
			Body: &icmp.Echo{ID: msgID, Seq: seq, Data: data},
		}).Marshal(nil)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	at := time.Now()

	parse := icmpEchoParser(mode, token, id, 10)
	if ev, ok := parse(reply(t, id, 3, token[:]), nil, at); !ok || ev.seq != 3 || !ev.at.Equal(at) {
		t.Errorf("valid raw reply = (%+v, %v), want seq 3", ev, ok)
	}
	if _, ok := parse(reply(t, id, 3, []byte("tokenXok")), nil, at); ok {
		t.Error("wrong token must not match")
	}
	if _, ok := parse(reply(t, id+1, 3, token[:]), nil, at); ok {
		t.Error("wrong ID must not match on a raw socket")
	}
	if _, ok := parse(reply(t, id, 0, token[:]), nil, at); ok {
		t.Error("seq below the train must not match")
	}
	if _, ok := parse(reply(t, id, 11, token[:]), nil, at); ok {
		t.Error("seq beyond the train must not match")
	}
	req, err := (&icmp.Message{Type: ipv4.ICMPTypeEcho, Body: &icmp.Echo{ID: id, Seq: 3, Data: token[:]}}).Marshal(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := parse(req, nil, at); ok {
		t.Error("an echo request must not match as a reply")
	}

	// Under datagram ICMP the kernel rewrites the echo ID, so it proves
	// nothing and must be ignored.
	dgram := mode
	dgram.raw = false
	parse = icmpEchoParser(dgram, token, id, 10)
	if _, ok := parse(reply(t, id+1, 3, token[:]), nil, at); !ok {
		t.Error("datagram mode must ignore the rewritten ID")
	}
}
