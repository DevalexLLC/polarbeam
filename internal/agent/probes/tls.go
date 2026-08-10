package probes

import (
	"context"
	stdtls "crypto/tls"
	"net"
	"strconv"
	"time"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
)

// TLS measures TCP connect plus TLS handshake against address:port.
//
// Params: "tls.sni" overrides the handshake server name (default: the target
// address); "tls.insecure_skip_verify"="true" skips chain verification (dev
// stacks with self-signed certs). A verification failure still reports its
// timings — the handshake was measured, it just failed.
type TLS struct{}

func (TLS) Run(ctx context.Context, spec *pb.ProbeSpec) *pb.ProbeResult {
	start := time.Now()
	res := newResult(spec, start)
	res.Sent = 1

	target := spec.GetTarget()
	addr := net.JoinHostPort(target.GetAddress(), strconv.Itoa(int(target.GetPort())))
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	connectDone := time.Now()
	if err != nil {
		return fail(res, err, pb.ProbeStatus_PROBE_STATUS_UNSPECIFIED)
	}
	defer conn.Close()
	res.Timings.TcpConnectUs = us(connectDone.Sub(start))

	params := spec.GetParams()
	serverName := params["tls.sni"]
	if serverName == "" {
		serverName = target.GetAddress()
	}
	tlsConn := stdtls.Client(conn, &stdtls.Config{
		ServerName:         serverName,
		InsecureSkipVerify: params["tls.insecure_skip_verify"] == "true",
	})
	err = tlsConn.HandshakeContext(ctx)
	handshakeDone := time.Now()
	res.Timings.TlsHandshakeUs = us(handshakeDone.Sub(connectDone))
	res.Timings.TotalUs = us(handshakeDone.Sub(start))
	if err != nil {
		return fail(res, err, pb.ProbeStatus_PROBE_STATUS_TLS_FAILURE)
	}

	res.Received = 1
	res.Status = pb.ProbeStatus_PROBE_STATUS_OK
	return res
}
