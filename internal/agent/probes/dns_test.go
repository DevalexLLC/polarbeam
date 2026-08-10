package probes

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"codeberg.org/miekg/dns"
	"codeberg.org/miekg/dns/dnsutil"
	"google.golang.org/protobuf/types/known/durationpb"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
)

// dnsResponder runs an in-process UDP resolver that answers every query with
// rcode, or black-holes queries entirely when drop is set.
func dnsResponder(t *testing.T, rcode uint16, drop bool) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
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
			q := new(dns.Msg)
			q.Data = append([]byte(nil), buf[:n]...)
			if err := q.Unpack(); err != nil {
				continue
			}
			r := dnsutil.SetReply(new(dns.Msg), q)
			r.Rcode = rcode
			if err := r.Pack(); err != nil {
				continue
			}
			pc.WriteTo(r.Data, peer)
		}
	}()
	return pc.LocalAddr().String()
}

func dnsSpec(resolverAddr string, params map[string]string) *pb.ProbeSpec {
	host, portStr, err := net.SplitHostPort(resolverAddr)
	if err != nil {
		panic(err)
	}
	port, _ := strconv.Atoi(portStr)
	return &pb.ProbeSpec{
		ProbeId: "test-dns",
		Type:    pb.ProbeType_PROBE_TYPE_DNS,
		Target: &pb.Target{
			Kind:     pb.TargetKind_TARGET_KIND_EXTERNAL,
			TargetId: "test-target",
			Address:  host,
			Port:     uint32(port),
		},
		Timeout: durationpb.New(2 * time.Second),
		Params:  params,
	}
}

func TestDNSOK(t *testing.T) {
	addr := dnsResponder(t, dns.RcodeSuccess, false)
	res := DNS{}.Run(context.Background(), dnsSpec(addr, map[string]string{"dns.qname": "example.org"}))
	if res.Status != pb.ProbeStatus_PROBE_STATUS_OK {
		t.Fatalf("status = %v, error = %q", res.Status, res.Error)
	}
	if res.Timings.DnsUs < 0 || res.Timings.TotalUs < 0 {
		t.Errorf("timings not measured: %+v", res.Timings)
	}
	if res.Sent != 1 || res.Received != 1 {
		t.Errorf("sent/received = %d/%d", res.Sent, res.Received)
	}
}

func TestDNSWrongRcode(t *testing.T) {
	addr := dnsResponder(t, dns.RcodeNameError, false)
	res := DNS{}.Run(context.Background(), dnsSpec(addr, map[string]string{"dns.qname": "example.org"}))
	if res.Status != pb.ProbeStatus_PROBE_STATUS_DNS_FAILURE {
		t.Errorf("status = %v, want DNS_FAILURE", res.Status)
	}
	if res.Error == "" || res.Received != 1 {
		t.Errorf("wrong-rcode answer: error=%q received=%d", res.Error, res.Received)
	}
}

func TestDNSExpectedRcode(t *testing.T) {
	addr := dnsResponder(t, dns.RcodeNameError, false)
	res := DNS{}.Run(context.Background(), dnsSpec(addr, map[string]string{
		"dns.qname":        "nope.example.org",
		"dns.expect_rcode": "NXDOMAIN",
	}))
	if res.Status != pb.ProbeStatus_PROBE_STATUS_OK {
		t.Fatalf("status = %v, error = %q", res.Status, res.Error)
	}
}

func TestDNSTimeout(t *testing.T) {
	addr := dnsResponder(t, dns.RcodeSuccess, true) // black hole
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	res := DNS{}.Run(ctx, dnsSpec(addr, map[string]string{"dns.qname": "example.org"}))
	if res.Status != pb.ProbeStatus_PROBE_STATUS_TIMEOUT {
		t.Errorf("status = %v (error %q), want TIMEOUT", res.Status, res.Error)
	}
}

func TestDNSParamValidation(t *testing.T) {
	cases := []struct {
		name   string
		params map[string]string
		errSub string
	}{
		{"missing qname", map[string]string{}, "dns.qname"},
		{"bad qtype", map[string]string{"dns.qname": "x.org", "dns.qtype": "BOGUS"}, "dns.qtype"},
		{"bad rcode", map[string]string{"dns.qname": "x.org", "dns.expect_rcode": "NOPE"}, "dns.expect_rcode"},
	}
	for _, c := range cases {
		res := DNS{}.Run(context.Background(), dnsSpec("127.0.0.1:1", c.params))
		if res.Status != pb.ProbeStatus_PROBE_STATUS_ERROR {
			t.Errorf("%s: status = %v, want ERROR", c.name, res.Status)
		}
		if !strings.Contains(res.Error, c.errSub) {
			t.Errorf("%s: error %q must name %q", c.name, res.Error, c.errSub)
		}
	}
}

func TestDNSQtypeTable(t *testing.T) {
	addr := dnsResponder(t, dns.RcodeSuccess, false)
	for _, qt := range []string{"A", "AAAA", "CNAME", "MX", "NS", "PTR", "SOA", "SRV", "TXT", "txt"} {
		res := DNS{}.Run(context.Background(), dnsSpec(addr, map[string]string{
			"dns.qname": "example.org", "dns.qtype": qt,
		}))
		if res.Status != pb.ProbeStatus_PROBE_STATUS_OK {
			t.Errorf("qtype %s: status = %v, error = %q", qt, res.Status, res.Error)
		}
	}
}
