package probes

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"strconv"
	"time"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
)

// NTP sends one SNTP client-mode request (the RFC 4330 subset of RFC 5905)
// over UDP and validates the reply: server mode, originate timestamp echoing
// the request, stratum 1-15, synchronized leap indicator, nonzero transmit
// timestamp. Kiss-o'-Death replies (stratum 0) report the four-character
// kiss code. The probe establishes NTP service reachability and round-trip
// time only — clock offset is never computed, so the request's transmit
// timestamp carries a random nonce instead of wall time and the originate
// check doubles as an off-path anti-spoofing guard.
type NTP struct{}

func (NTP) Run(ctx context.Context, spec *pb.ProbeSpec) *pb.ProbeResult {
	start := time.Now()
	res := newResult(spec, start)
	res.Sent = 1

	// LI=0 VN=4 Mode=3 (client) with every other field zero is a complete
	// client request. Bytes 40:48 are the transmit timestamp (64-bit fixed
	// point: seconds since 1900 + fraction) — an opaque nonce to the server,
	// which must echo it into the originate field of its reply.
	req := make([]byte, 48)
	req[0] = 0x23
	nonce := req[40:48]
	if _, err := rand.Read(nonce); err != nil {
		res.Status = pb.ProbeStatus_PROBE_STATUS_ERROR
		res.Error = fmt.Sprintf("random nonce: %v", err)
		return res
	}

	ip, err := resolveIP(ctx, spec.GetTarget().GetAddress())
	if err != nil {
		return fail(res, err, pb.ProbeStatus_PROBE_STATUS_UNSPECIFIED)
	}
	port := int(spec.GetTarget().GetPort())
	if port == 0 {
		port = 123
	}
	// Dialing the resolved literal keeps resolution failures cleanly in the
	// DNS_FAILURE bucket above; a connected UDP socket also kernel-filters
	// datagrams from any other source address.
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", net.JoinHostPort(ip.String(), strconv.Itoa(port)))
	if err != nil {
		return fail(res, err, pb.ProbeStatus_PROBE_STATUS_UNSPECIFIED)
	}
	defer conn.Close()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = start.Add(5 * time.Second)
	}
	conn.SetDeadline(deadline)
	// The blocking read below cannot observe ctx on its own; without this,
	// a worker stopped mid-run (shutdown, reconfigure) would hold the
	// scheduler's WaitGroup until the socket deadline fires.
	stopCancelWatch := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Now()) })
	defer stopCancelWatch()

	// Single shot, no retransmission: exactly one packet per interval keeps
	// the load on shared time servers minimal; the schedule is the retry.
	sendAt := time.Now()
	if _, err := conn.Write(req); err != nil {
		return fail(res, err, pb.ProbeStatus_PROBE_STATUS_UNSPECIFIED)
	}
	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	rtt := time.Since(sendAt)
	if err != nil {
		return fail(res, fmt.Errorf("no NTP response from %s: %w", conn.RemoteAddr(), err),
			pb.ProbeStatus_PROBE_STATUS_UNSPECIFIED)
	}

	res.Received = 1
	res.Timings.TotalUs = us(time.Since(start))
	if msg := validateNTPResponse(buf[:n], nonce); msg != "" {
		res.Status = pb.ProbeStatus_PROBE_STATUS_ERROR
		res.Error = msg
		return res
	}
	r := us(rtt)
	res.Rtt = &pb.RttStats{MinUs: r, AvgUs: r, MaxUs: r, StddevUs: 0}
	res.Status = pb.ProbeStatus_PROBE_STATUS_OK
	return res
}

// validateNTPResponse checks a received datagram against the request nonce,
// returning "" when the reply proves a synchronized NTP server answered us.
// Order matters: structure first, authenticity next (a verdict on a packet
// that fails the originate check is meaningless), then Kiss-o'-Death before
// the stratum range (stratum 0 IS the KoD signal, not "out of range") and
// before the leap check (KoD packets legitimately carry LI=3). The version
// field is deliberately not checked — servers echo the request's version.
func validateNTPResponse(resp, nonce []byte) string {
	if len(resp) < 48 {
		return fmt.Sprintf("short NTP response: %d bytes (want 48)", len(resp))
	}
	if mode := resp[0] & 0x07; mode != 4 {
		return fmt.Sprintf("NTP response mode %d (want 4 server)", mode)
	}
	if !bytes.Equal(resp[24:32], nonce) {
		return "NTP originate timestamp does not match request transmit timestamp"
	}
	if resp[1] == 0 {
		return fmt.Sprintf("NTP kiss-o'-death %q: server refuses service; reduce polling rate or use another server", resp[12:16])
	}
	if resp[1] > 15 {
		return fmt.Sprintf("NTP stratum %d out of range (want 1-15)", resp[1])
	}
	if resp[0]>>6 == 3 {
		return "NTP server unsynchronized (leap indicator 3)"
	}
	var zero [8]byte
	if bytes.Equal(resp[40:48], zero[:]) {
		return "NTP response transmit timestamp is zero"
	}
	return ""
}
