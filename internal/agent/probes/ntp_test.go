package probes

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
)

// ntpResponder runs an in-process SNTP server on the loopback of network
// ("udp" or "udp6") that answers every request with a valid 48-byte server
// reply after applying muts; drop black-holes requests entirely and a
// respLen in (0, 48) truncates the reply.
func ntpResponder(t *testing.T, network string, drop bool, respLen int, muts ...func(req, resp []byte)) string {
	t.Helper()
	addr := "127.0.0.1:0"
	if network == "udp6" {
		addr = "[::1]:0"
	}
	pc, err := net.ListenPacket(network, addr)
	if err != nil {
		if network == "udp6" {
			t.Skipf("no IPv6 loopback: %v", err)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, peer, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if drop {
				continue
			}
			resp := make([]byte, 48)
			resp[0] = 0x24 // LI=0, VN=4, Mode=4 (server)
			resp[1] = 2    // stratum
			copy(resp[12:16], "LOCL")
			if n >= 48 {
				copy(resp[24:32], buf[40:48]) // originate = request transmit
			}
			copy(resp[32:40], []byte{0xEA, 1, 2, 3, 4, 5, 6, 7})      // receive timestamp
			copy(resp[40:48], []byte{0xEA, 8, 9, 10, 11, 12, 13, 14}) // transmit timestamp
			for _, m := range muts {
				m(buf[:n], resp)
			}
			if respLen > 0 && respLen < len(resp) {
				resp = resp[:respLen]
			}
			pc.WriteTo(resp, peer)
		}
	}()
	return pc.LocalAddr().String()
}

func ntpSpec(addr string) *pb.ProbeSpec {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		panic(err)
	}
	port, _ := strconv.Atoi(portStr)
	return &pb.ProbeSpec{
		ProbeId: "test-ntp",
		Type:    pb.ProbeType_PROBE_TYPE_NTP,
		Target: &pb.Target{
			Kind:     pb.TargetKind_TARGET_KIND_EXTERNAL,
			TargetId: "test-target",
			Address:  host,
			Port:     uint32(port),
		},
		Timeout: durationpb.New(2 * time.Second),
	}
}

func TestNTPOK(t *testing.T) {
	addr := ntpResponder(t, "udp", false, 0)
	res := NTP{}.Run(context.Background(), ntpSpec(addr))
	if res.Status != pb.ProbeStatus_PROBE_STATUS_OK {
		t.Fatalf("status = %v, error = %q", res.Status, res.Error)
	}
	if res.Sent != 1 || res.Received != 1 {
		t.Errorf("sent/received = %d/%d", res.Sent, res.Received)
	}
	if res.Rtt.MinUs < 0 || res.Rtt.AvgUs < 0 || res.Rtt.MaxUs < 0 || res.Rtt.StddevUs != 0 {
		t.Errorf("rtt not measured as single sample: %+v", res.Rtt)
	}
	if res.Rtt.MinUs != res.Rtt.AvgUs || res.Rtt.AvgUs != res.Rtt.MaxUs {
		t.Errorf("single-sample rtt must have min=avg=max: %+v", res.Rtt)
	}
	if res.Timings.TotalUs < 0 {
		t.Errorf("total not measured: %+v", res.Timings)
	}
}

func TestNTPOKIPv6(t *testing.T) {
	addr := ntpResponder(t, "udp6", false, 0)
	res := NTP{}.Run(context.Background(), ntpSpec(addr))
	if res.Status != pb.ProbeStatus_PROBE_STATUS_OK {
		t.Fatalf("status = %v, error = %q", res.Status, res.Error)
	}
}

func TestNTPTimeout(t *testing.T) {
	addr := ntpResponder(t, "udp", true, 0) // black hole
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	res := NTP{}.Run(ctx, ntpSpec(addr))
	if res.Status != pb.ProbeStatus_PROBE_STATUS_TIMEOUT {
		t.Errorf("status = %v (error %q), want TIMEOUT", res.Status, res.Error)
	}
	if !strings.Contains(res.Error, "no NTP response") {
		t.Errorf("error %q must name the missing response", res.Error)
	}
	if res.Received != 0 {
		t.Errorf("received = %d, want 0", res.Received)
	}
}

// TestNTPCancelUnblocksRead pins that canceling the run context interrupts
// the blocking UDP read immediately: a worker stopped mid-run (shutdown or
// reconfigure) must not hold the scheduler until the probe timeout fires.
func TestNTPCancelUnblocksRead(t *testing.T) {
	addr := ntpResponder(t, "udp", true, 0) // black hole
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	done := make(chan *pb.ProbeResult, 1)
	go func() { done <- NTP{}.Run(ctx, ntpSpec(addr)) }()
	select {
	case res := <-done:
		if res.Status == pb.ProbeStatus_PROBE_STATUS_OK {
			t.Errorf("status = %v, want a failure after cancellation", res.Status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run still blocked 2s after cancellation")
	}
}

func TestNTPConnRefused(t *testing.T) {
	// Grab a loopback UDP port and close it: the write draws ICMP
	// port-unreachable, which the connected socket reports as ECONNREFUSED
	// on the read.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := pc.LocalAddr().String()
	pc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res := NTP{}.Run(ctx, ntpSpec(addr))
	if res.Status != pb.ProbeStatus_PROBE_STATUS_CONN_REFUSED {
		t.Errorf("status = %v (error %q), want CONN_REFUSED", res.Status, res.Error)
	}
}

func TestNTPDNSFailure(t *testing.T) {
	spec := ntpSpec("127.0.0.1:123")
	spec.Target.Address = "ntp.invalid." // RFC 6761 reserved, never resolves
	res := NTP{}.Run(context.Background(), spec)
	if res.Status != pb.ProbeStatus_PROBE_STATUS_DNS_FAILURE {
		t.Errorf("status = %v (error %q), want DNS_FAILURE", res.Status, res.Error)
	}
}

func TestNTPResponseValidation(t *testing.T) {
	cases := []struct {
		name    string
		respLen int
		mut     func(req, resp []byte)
		errSub  string
	}{
		{"short packet", 20, nil, "short NTP response: 20 bytes"},
		{"wrong mode", 0, func(req, resp []byte) { resp[0] = 0x23 }, "mode 3 (want 4 server)"},
		{"origin mismatch", 0, func(req, resp []byte) { resp[24] ^= 0xFF }, "originate timestamp"},
		{"kiss-o'-death", 0, func(req, resp []byte) {
			resp[1] = 0
			copy(resp[12:16], "RATE")
		}, `kiss-o'-death "RATE"`},
		{"stratum too high", 0, func(req, resp []byte) { resp[1] = 16 }, "stratum 16 out of range"},
		{"unsynchronized", 0, func(req, resp []byte) { resp[0] |= 0xC0 }, "unsynchronized (leap indicator 3)"},
		{"zero transmit", 0, func(req, resp []byte) {
			for i := 40; i < 48; i++ {
				resp[i] = 0
			}
		}, "transmit timestamp is zero"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var muts []func(req, resp []byte)
			if c.mut != nil {
				muts = append(muts, c.mut)
			}
			addr := ntpResponder(t, "udp", false, c.respLen, muts...)
			res := NTP{}.Run(context.Background(), ntpSpec(addr))
			if res.Status != pb.ProbeStatus_PROBE_STATUS_ERROR {
				t.Fatalf("status = %v (error %q), want ERROR", res.Status, res.Error)
			}
			if !strings.Contains(res.Error, c.errSub) {
				t.Errorf("error %q must contain %q", res.Error, c.errSub)
			}
			if res.Received != 1 {
				t.Errorf("received = %d, want 1 (a datagram arrived)", res.Received)
			}
		})
	}
}
