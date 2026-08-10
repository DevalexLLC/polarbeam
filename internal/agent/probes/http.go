package probes

import (
	"context"
	stdtls "crypto/tls"
	"fmt"
	"io"
	stdhttp "net/http"
	"net/http/httptrace"
	"time"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
)

// HTTP measures a full request against the target URL with per-phase timings
// from httptrace (dns, tcp connect, tls handshake, ttfb) and asserts the
// response status.
//
// Params: "http.method" (default GET), "http.expect_status" — an exact code
// ("200") or a class ("2xx"), default "200" — and
// "http.insecure_skip_verify"="true" for self-signed endpoints. Redirects
// are NOT followed: the probe measures the configured endpoint, not wherever
// it redirects to.
type HTTP struct{}

// httpBodyLimit caps how much of the body the probe drains: enough to
// complete small responses, bounded so a huge download can't stall a worker.
const httpBodyLimit = 1 << 20

func (HTTP) Run(ctx context.Context, spec *pb.ProbeSpec) *pb.ProbeResult {
	start := time.Now()
	res := newResult(spec, start)
	res.Sent = 1
	params := spec.GetParams()

	var dnsStart, dnsDone, connectStart, connectDone, tlsStart, tlsDone, firstByte time.Time
	trace := &httptrace.ClientTrace{
		DNSStart:             func(httptrace.DNSStartInfo) { dnsStart = time.Now() },
		DNSDone:              func(httptrace.DNSDoneInfo) { dnsDone = time.Now() },
		ConnectStart:         func(string, string) { connectStart = time.Now() },
		ConnectDone:          func(string, string, error) { connectDone = time.Now() },
		TLSHandshakeStart:    func() { tlsStart = time.Now() },
		TLSHandshakeDone:     func(stdtls.ConnectionState, error) { tlsDone = time.Now() },
		GotFirstResponseByte: func() { firstByte = time.Now() },
	}

	method := params["http.method"]
	if method == "" {
		method = stdhttp.MethodGet
	}
	req, err := stdhttp.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace),
		method, spec.GetTarget().GetUrl(), nil)
	if err != nil {
		return fail(res, err, pb.ProbeStatus_PROBE_STATUS_UNSPECIFIED)
	}

	client := &stdhttp.Client{
		Transport: &stdhttp.Transport{
			DisableKeepAlives: true,
			TLSClientConfig: &stdtls.Config{
				InsecureSkipVerify: params["http.insecure_skip_verify"] == "true",
			},
		},
		CheckRedirect: func(*stdhttp.Request, []*stdhttp.Request) error {
			return stdhttp.ErrUseLastResponse
		},
	}

	record := func() {
		if !dnsStart.IsZero() && !dnsDone.IsZero() {
			res.Timings.DnsUs = us(dnsDone.Sub(dnsStart))
		}
		if !connectStart.IsZero() && !connectDone.IsZero() {
			res.Timings.TcpConnectUs = us(connectDone.Sub(connectStart))
		}
		if !tlsStart.IsZero() && !tlsDone.IsZero() {
			res.Timings.TlsHandshakeUs = us(tlsDone.Sub(tlsStart))
		}
		if !firstByte.IsZero() {
			res.Timings.TtfbUs = us(firstByte.Sub(start))
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		record()
		res.Timings.TotalUs = us(time.Since(start))
		return fail(res, err, pb.ProbeStatus_PROBE_STATUS_UNSPECIFIED)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, httpBodyLimit))
	resp.Body.Close()
	record()
	res.Timings.TotalUs = us(time.Since(start))
	res.Received = 1

	expect := params["http.expect_status"]
	if expect == "" {
		expect = "200"
	}
	if !statusMatches(expect, resp.StatusCode) {
		res.Status = pb.ProbeStatus_PROBE_STATUS_ERROR
		res.Error = fmt.Sprintf("expected status %s, got %d", expect, resp.StatusCode)
		return res
	}
	res.Status = pb.ProbeStatus_PROBE_STATUS_OK
	return res
}

// statusMatches accepts an exact code ("200") or a class form ("2xx").
func statusMatches(expect string, code int) bool {
	if len(expect) == 3 && expect[1] == 'x' && expect[2] == 'x' {
		return expect[0] == byte('0'+code/100)
	}
	return expect == fmt.Sprintf("%d", code)
}
