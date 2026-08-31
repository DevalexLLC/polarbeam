package probes

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"time"

	"golang.org/x/net/icmp"
)

// This file holds the run plumbing shared by the three raw-socket probers
// (icmp, traceroute, pathmtu): deadline derivation, the per-run echo
// token, the reader goroutine, per-round wait budgeting, and the quoted
// IP-header arithmetic of ICMP error bodies. Socket opening stays in each
// prober: the three paths are genuinely different (datagram-then-raw
// fallback, raw + a second UDP sender, raw *net.IPConn for the PMTU
// socket options) and each owns its capability error message.

// sessionConn is the slice of a packet conn the reader session needs.
// Satisfied by *icmp.PacketConn and *net.IPConn, and by a fake in tests.
type sessionConn interface {
	ReadFrom(b []byte) (n int, addr net.Addr, err error)
	SetReadDeadline(t time.Time) error
}

// icmpReadSession arms conn's read deadline (error deliberately
// discarded: the deadline is a backstop, and the reader exits through the
// caller's deferred Close either way) and starts the reader goroutine:
// read until the first error, hand each packet to parse, and deliver
// matched events without ever blocking. The goroutine is never joined; it
// exits when the caller's deferred Close (or the read deadline) fails the
// blocking read.
func icmpReadSession[E any](conn sessionConn, deadline time.Time, bufSize, chanCap int,
	parse func(pkt []byte, peer net.Addr, at time.Time) (E, bool),
) <-chan E {
	conn.SetReadDeadline(deadline)
	events := make(chan E, chanCap)
	go func() {
		buf := make([]byte, bufSize)
		for {
			n, peer, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			ev, ok := parse(buf[:n], peer, time.Now())
			if !ok {
				continue
			}
			select { // never block: the run may already be over
			case events <- ev:
			default:
			}
		}
	}()
	return events
}

// runDeadline is the run's absolute deadline: ctx's when set, else
// start+fallback.
func runDeadline(ctx context.Context, start time.Time, fallback time.Duration) time.Time {
	if d, ok := ctx.Deadline(); ok {
		return d
	}
	return start.Add(fallback)
}

// echoToken returns the random per-run token and the echo ID derived from
// its first two bytes. The error is pre-formatted so callers assign
// err.Error() to the result verbatim.
func echoToken() (token [8]byte, id int, err error) {
	if _, err := rand.Read(token[:]); err != nil {
		return token, 0, fmt.Errorf("random token: %v", err)
	}
	return token, int(binary.BigEndian.Uint16(token[:2])), nil
}

// budget is the per-round wait so the remaining rounds fit inside the run
// deadline: nominal, shrunk to remaining/rounds when the budget is
// tighter. A floor keeps a tight budget from misreading a slow reply as
// silence, clamped so the floor never overshoots the deadline itself.
// Both floor and clamp apply only when a floor is given: the floor-less
// caller (traceroute) historically returns the shrunk wait unchanged even
// when remaining has gone negative, and must keep doing so.
func budget(remaining time.Duration, rounds int, nominal, floor time.Duration) time.Duration {
	wait := nominal
	if per := remaining / time.Duration(rounds); per < wait {
		wait = per
	}
	if floor > 0 {
		if wait < floor {
			wait = floor
		}
		if wait > remaining {
			wait = remaining
		}
	}
	return wait
}

// quotedInner dissects the quoted original datagram inside an ICMP error
// body: the quoted IP header's protocol byte, destination address, and
// the 8 bytes immediately past the IP header — all an RFC 792 error is
// guaranteed to quote (the UDP header for traceroute, the echo header for
// pathmtu). Only the length and header-length checks are shared; what to
// make of proto and dst is the caller's business (traceroute historically
// never checks proto — keep it that way). The v6 arm assumes a bare
// 40-byte header with no extension headers, as both callers always have.
func quotedInner(v4 bool, quoted []byte) (proto byte, dst net.IP, inner []byte, ok bool) {
	if v4 {
		if len(quoted) < ipv4HeaderLen {
			return 0, nil, nil, false
		}
		hl := int(quoted[0]&0x0f) * 4
		if hl < ipv4HeaderLen || len(quoted) < hl+8 {
			return 0, nil, nil, false
		}
		return quoted[9], net.IP(quoted[16:20]), quoted[hl : hl+8], true
	}
	if len(quoted) < ipv6HeaderLen+8 {
		return 0, nil, nil, false
	}
	return quoted[6], net.IP(quoted[24:40]), quoted[40:48], true
}

// icmpReply is one matched echo reply from the icmp prober's reader.
type icmpReply struct {
	seq int
	at  time.Time
}

// icmpEchoParser is the icmp prober's reader parse function: an echo
// reply carrying exactly the run token, the run ID when the socket is raw
// (under datagram ICMP the kernel rewrites the ID, so it proves nothing),
// and a seq within the train.
func icmpEchoParser(mode icmpMode, token [8]byte, id, count int) func(pkt []byte, peer net.Addr, at time.Time) (icmpReply, bool) {
	return func(pkt []byte, _ net.Addr, at time.Time) (icmpReply, bool) {
		msg, err := icmp.ParseMessage(mode.proto, pkt)
		if err != nil || msg.Type != mode.echoReply {
			return icmpReply{}, false
		}
		echo, ok := msg.Body.(*icmp.Echo)
		if !ok || string(echo.Data) != string(token[:]) {
			return icmpReply{}, false
		}
		if mode.raw && echo.ID != id {
			return icmpReply{}, false
		}
		if echo.Seq < 1 || echo.Seq > count {
			return icmpReply{}, false
		}
		return icmpReply{seq: echo.Seq, at: at}, true
	}
}
