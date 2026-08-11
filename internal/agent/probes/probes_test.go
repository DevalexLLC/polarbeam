package probes

import (
	"context"
	stdtls "crypto/tls"
	"errors"
	"net"
	stdhttp "net/http"
	"net/http/httptest"
	"strconv"
	"syscall"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
)

func tcpSpec(address string, port int) *pb.ProbeSpec {
	return &pb.ProbeSpec{
		ProbeId: "test-probe",
		Type:    pb.ProbeType_PROBE_TYPE_TCP,
		Target: &pb.Target{
			Kind:     pb.TargetKind_TARGET_KIND_EXTERNAL,
			TargetId: "test-target",
			Address:  address,
			Port:     uint32(port),
		},
		Interval: durationpb.New(time.Second),
		Timeout:  durationpb.New(2 * time.Second),
	}
}

func splitAddr(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portStr)
	return host, port
}

func TestTCPOK(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	host, port := splitAddr(t, ln.Addr().String())
	res := TCP{}.Run(context.Background(), tcpSpec(host, port))
	if res.Status != pb.ProbeStatus_PROBE_STATUS_OK {
		t.Fatalf("status = %v, error = %q", res.Status, res.Error)
	}
	if res.Timings.TcpConnectUs < 0 || res.Timings.TotalUs < 0 {
		t.Errorf("timings not measured: %+v", res.Timings)
	}
	if res.Timings.TlsHandshakeUs != -1 || res.Timings.DnsUs != -1 {
		t.Errorf("unmeasured phases must stay -1: %+v", res.Timings)
	}
	if res.Sent != 1 || res.Received != 1 {
		t.Errorf("sent/received = %d/%d", res.Sent, res.Received)
	}
	if res.ProbeId != "test-probe" || res.TargetId != "test-target" {
		t.Errorf("identity not stamped: %s/%s", res.ProbeId, res.TargetId)
	}
}

func TestTCPConnRefused(t *testing.T) {
	// Grab a port and close the listener so nothing is listening on it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, port := splitAddr(t, ln.Addr().String())
	ln.Close()

	res := TCP{}.Run(context.Background(), tcpSpec(host, port))
	if res.Status != pb.ProbeStatus_PROBE_STATUS_CONN_REFUSED {
		t.Errorf("status = %v (error %q), want CONN_REFUSED", res.Status, res.Error)
	}
	if res.Error == "" {
		t.Error("failure must carry an error message")
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		err  error
		want pb.ProbeStatus
	}{
		{context.DeadlineExceeded, pb.ProbeStatus_PROBE_STATUS_TIMEOUT},
		{&net.OpError{Err: &timeoutErr{}}, pb.ProbeStatus_PROBE_STATUS_TIMEOUT},
		{syscall.ECONNREFUSED, pb.ProbeStatus_PROBE_STATUS_CONN_REFUSED},
		{&net.DNSError{Err: "no such host"}, pb.ProbeStatus_PROBE_STATUS_DNS_FAILURE},
		{&stdtls.CertificateVerificationError{Err: errors.New("bad cert")}, pb.ProbeStatus_PROBE_STATUS_TLS_FAILURE},
		{errors.New("anything else"), pb.ProbeStatus_PROBE_STATUS_ERROR},
	}
	for _, c := range cases {
		if got := classify(c.err); got != c.want {
			t.Errorf("classify(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

type timeoutErr struct{}

func (*timeoutErr) Error() string   { return "timeout" }
func (*timeoutErr) Timeout() bool   { return true }
func (*timeoutErr) Temporary() bool { return true }

func TestTLSOKAndVerifyFailure(t *testing.T) {
	// httptest.NewTLSServer gives us a listener with a self-signed cert.
	srv := httptest.NewTLSServer(nil)
	defer srv.Close()
	host, port := splitAddr(t, srv.Listener.Addr().String())

	spec := tcpSpec(host, port)
	spec.Type = pb.ProbeType_PROBE_TYPE_TLS

	// Without skip-verify: the self-signed cert must fail as TLS_FAILURE,
	// with handshake timing still measured.
	res := TLS{}.Run(context.Background(), spec)
	if res.Status != pb.ProbeStatus_PROBE_STATUS_TLS_FAILURE {
		t.Errorf("status = %v (error %q), want TLS_FAILURE", res.Status, res.Error)
	}
	if res.Timings.TlsHandshakeUs < 0 {
		t.Error("failed handshake must still report its timing")
	}

	spec.Params = map[string]string{"tls.insecure_skip_verify": "true"}
	res = TLS{}.Run(context.Background(), spec)
	if res.Status != pb.ProbeStatus_PROBE_STATUS_OK {
		t.Fatalf("status = %v, error = %q", res.Status, res.Error)
	}
	if res.Timings.TcpConnectUs < 0 || res.Timings.TlsHandshakeUs < 0 || res.Timings.TotalUs < 0 {
		t.Errorf("timings not measured: %+v", res.Timings)
	}
}

func httpSpec(url string, params map[string]string) *pb.ProbeSpec {
	return &pb.ProbeSpec{
		ProbeId: "test-http",
		Type:    pb.ProbeType_PROBE_TYPE_HTTP,
		Target: &pb.Target{
			Kind:     pb.TargetKind_TARGET_KIND_EXTERNAL,
			TargetId: "test-target",
			Url:      url,
		},
		Timeout: durationpb.New(2 * time.Second),
		Params:  params,
	}
}

func TestHTTPOK(t *testing.T) {
	srv := httptest.NewServer(nil) // 404 for every path
	defer srv.Close()

	res := HTTP{}.Run(context.Background(), httpSpec(srv.URL, map[string]string{"http.expect_status": "404"}))
	if res.Status != pb.ProbeStatus_PROBE_STATUS_OK {
		t.Fatalf("status = %v, error = %q", res.Status, res.Error)
	}
	if res.Timings.TcpConnectUs < 0 || res.Timings.TtfbUs < 0 || res.Timings.TotalUs < 0 {
		t.Errorf("timings not measured: %+v", res.Timings)
	}
	if res.Timings.TtfbUs > res.Timings.TotalUs {
		t.Errorf("ttfb (%d) > total (%d)", res.Timings.TtfbUs, res.Timings.TotalUs)
	}
	if res.Timings.TlsHandshakeUs != -1 {
		t.Errorf("plain http must not report a TLS handshake: %d", res.Timings.TlsHandshakeUs)
	}
}

func TestHTTPStatusMismatch(t *testing.T) {
	srv := httptest.NewServer(nil) // 404
	defer srv.Close()

	res := HTTP{}.Run(context.Background(), httpSpec(srv.URL, nil)) // expects 200
	if res.Status != pb.ProbeStatus_PROBE_STATUS_ERROR {
		t.Errorf("status = %v, want ERROR", res.Status)
	}
	if res.Error == "" {
		t.Error("mismatch must explain itself in error text")
	}
	// The response WAS received; only the assertion failed.
	if res.Received != 1 {
		t.Errorf("received = %d, want 1", res.Received)
	}
}

func TestHTTPSInsecureAndClassExpect(t *testing.T) {
	srv := httptest.NewTLSServer(nil) // 404, self-signed
	defer srv.Close()

	params := map[string]string{
		"http.insecure_skip_verify": "true",
		"http.expect_status":        "4xx",
	}
	res := HTTP{}.Run(context.Background(), httpSpec(srv.URL, params))
	if res.Status != pb.ProbeStatus_PROBE_STATUS_OK {
		t.Fatalf("status = %v, error = %q", res.Status, res.Error)
	}
	if res.Timings.TlsHandshakeUs < 0 {
		t.Error("https probe must measure the TLS handshake")
	}

	// Without skip-verify the self-signed chain must classify as TLS_FAILURE.
	res = HTTP{}.Run(context.Background(), httpSpec(srv.URL, map[string]string{"http.expect_status": "4xx"}))
	if res.Status != pb.ProbeStatus_PROBE_STATUS_TLS_FAILURE {
		t.Errorf("status = %v (error %q), want TLS_FAILURE", res.Status, res.Error)
	}
}

func TestHTTPTruncatedBody(t *testing.T) {
	// Declare a 1000-byte body, send a fragment, drop the connection: the
	// probe must fail loud instead of reporting a received response.
	srv := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		conn, bufrw, err := w.(stdhttp.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		bufrw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 1000\r\n\r\nshort")
		bufrw.Flush()
		conn.Close()
	}))
	defer srv.Close()

	res := HTTP{}.Run(context.Background(), httpSpec(srv.URL, nil))
	if res.Status != pb.ProbeStatus_PROBE_STATUS_ERROR {
		t.Errorf("status = %v (error %q), want ERROR", res.Status, res.Error)
	}
	if res.Error == "" {
		t.Error("truncated body must carry an error message")
	}
	if res.Received != 0 {
		t.Errorf("received = %d, want 0", res.Received)
	}
}

func TestHTTPStalledBody(t *testing.T) {
	// Headers arrive, then the body never does: the run deadline must turn
	// the stalled read into TIMEOUT, not OK.
	srv := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(stdhttp.StatusOK)
		w.(stdhttp.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	res := HTTP{}.Run(ctx, httpSpec(srv.URL, nil))
	if res.Status != pb.ProbeStatus_PROBE_STATUS_TIMEOUT {
		t.Errorf("status = %v (error %q), want TIMEOUT", res.Status, res.Error)
	}
	if res.Received != 0 {
		t.Errorf("received = %d, want 0", res.Received)
	}
	if res.Timings.TtfbUs < 0 {
		t.Error("headers arrived, ttfb must be measured")
	}
}

func TestHTTPBodyCapIsOK(t *testing.T) {
	// A body larger than httpBodyLimit stops at the cap via clean EOF; that
	// is success, not a read error.
	srv := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		w.Write(make([]byte, 2*httpBodyLimit))
	}))
	defer srv.Close()

	res := HTTP{}.Run(context.Background(), httpSpec(srv.URL, nil))
	if res.Status != pb.ProbeStatus_PROBE_STATUS_OK {
		t.Fatalf("status = %v, error = %q", res.Status, res.Error)
	}
	if res.Received != 1 {
		t.Errorf("received = %d, want 1", res.Received)
	}
}

func TestStatusMatches(t *testing.T) {
	cases := []struct {
		expect string
		code   int
		want   bool
	}{
		{"200", 200, true}, {"200", 201, false},
		{"2xx", 204, true}, {"2xx", 301, false},
		{"5xx", 503, true}, {"", 200, false},
	}
	for _, c := range cases {
		if got := statusMatches(c.expect, c.code); got != c.want {
			t.Errorf("statusMatches(%q, %d) = %v", c.expect, c.code, got)
		}
	}
}
