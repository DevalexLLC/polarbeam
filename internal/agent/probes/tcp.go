package probes

import (
	"context"
	"net"
	"strconv"
	"time"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
)

// TCP measures a timed connect to target address:port. DNS resolution (when
// the address is a hostname) is included in tcp_connect_us, matching what a
// client application experiences.
type TCP struct{}

func (TCP) Run(ctx context.Context, spec *pb.ProbeSpec) *pb.ProbeResult {
	start := time.Now()
	res := newResult(spec, start)
	res.Sent = 1

	addr := net.JoinHostPort(spec.GetTarget().GetAddress(), strconv.Itoa(int(spec.GetTarget().GetPort())))
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	elapsed := time.Since(start)
	if err != nil {
		return fail(res, err, pb.ProbeStatus_PROBE_STATUS_UNSPECIFIED)
	}
	conn.Close()

	res.Received = 1
	res.Status = pb.ProbeStatus_PROBE_STATUS_OK
	res.Timings.TcpConnectUs = us(elapsed)
	res.Timings.TotalUs = us(elapsed)
	return res
}
