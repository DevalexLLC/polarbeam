package probes

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"codeberg.org/miekg/dns"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
)

// dnsQTypes is the deliberately small allowlist of query types the prober
// supports; unknown types are a loud ERROR, never a silent default.
var dnsQTypes = map[string]uint16{
	"A":     dns.TypeA,
	"AAAA":  dns.TypeAAAA,
	"CNAME": dns.TypeCNAME,
	"MX":    dns.TypeMX,
	"NS":    dns.TypeNS,
	"PTR":   dns.TypePTR,
	"SOA":   dns.TypeSOA,
	"SRV":   dns.TypeSRV,
	"TXT":   dns.TypeTXT,
}

var dnsRcodes = map[string]uint16{
	"NOERROR":  dns.RcodeSuccess,
	"FORMERR":  dns.RcodeFormatError,
	"SERVFAIL": dns.RcodeServerFailure,
	"NXDOMAIN": dns.RcodeNameError,
	"NOTIMPL":  dns.RcodeNotImplemented,
	"REFUSED":  dns.RcodeRefused,
}

// DNS queries the target as a resolver: params dns.qname (required),
// dns.qtype (default A), dns.resolver (overrides the target address) and
// dns.expect_rcode (default NOERROR). An answer with an unexpected RCODE is
// DNS_FAILURE; the exchange duration is both dns_us and total_us.
type DNS struct{}

func (DNS) Run(ctx context.Context, spec *pb.ProbeSpec) *pb.ProbeResult {
	start := time.Now()
	res := newResult(spec, start)
	res.Sent = 1
	params := spec.GetParams()

	qname := params["dns.qname"]
	if qname == "" {
		res.Status = pb.ProbeStatus_PROBE_STATUS_ERROR
		res.Error = "missing dns.qname param"
		return res
	}
	qtypeName := strings.ToUpper(params["dns.qtype"])
	if qtypeName == "" {
		qtypeName = "A"
	}
	qtype, ok := dnsQTypes[qtypeName]
	if !ok {
		res.Status = pb.ProbeStatus_PROBE_STATUS_ERROR
		res.Error = fmt.Sprintf("unsupported dns.qtype %q", params["dns.qtype"])
		return res
	}
	wantName := strings.ToUpper(params["dns.expect_rcode"])
	if wantName == "" {
		wantName = "NOERROR"
	}
	wantRcode, ok := dnsRcodes[wantName]
	if !ok {
		res.Status = pb.ProbeStatus_PROBE_STATUS_ERROR
		res.Error = fmt.Sprintf("unsupported dns.expect_rcode %q", params["dns.expect_rcode"])
		return res
	}
	resolver := params["dns.resolver"]
	if resolver == "" {
		port := int(spec.GetTarget().GetPort())
		if port == 0 {
			port = 53
		}
		resolver = net.JoinHostPort(spec.GetTarget().GetAddress(), strconv.Itoa(port))
	}

	msg := dns.NewMsg(qname, qtype)
	if msg == nil {
		res.Status = pb.ProbeStatus_PROBE_STATUS_ERROR
		res.Error = fmt.Sprintf("cannot build query for qtype %q", qtypeName)
		return res
	}
	// The library's default transport uses fixed 2 s read/write timeouts and
	// does not honor the ctx deadline mid-exchange; derive them from the run
	// deadline so spec.timeout is the single budget that matters.
	timeout := 5 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
	}
	client := &dns.Client{Transport: &dns.Transport{
		Dialer:       &net.Dialer{Timeout: timeout},
		ReadTimeout:  timeout,
		WriteTimeout: timeout,
	}}
	reply, _, err := client.Exchange(ctx, msg, "udp", resolver)
	elapsed := time.Since(start)
	if err != nil {
		return fail(res, err, pb.ProbeStatus_PROBE_STATUS_DNS_FAILURE)
	}

	res.Received = 1
	res.Timings.DnsUs = us(elapsed)
	res.Timings.TotalUs = us(elapsed)
	if reply.Rcode != wantRcode {
		res.Status = pb.ProbeStatus_PROBE_STATUS_DNS_FAILURE
		res.Error = fmt.Sprintf("RCODE %s (want %s)", rcodeName(reply.Rcode), wantName)
		return res
	}
	res.Status = pb.ProbeStatus_PROBE_STATUS_OK
	return res
}

func rcodeName(rcode uint16) string {
	if name, ok := dns.RcodeToString[rcode]; ok {
		return name
	}
	return strconv.Itoa(int(rcode))
}
