// Package probes implements the agent's probe engine: a Prober per probe
// type behind a registry keyed by wire ProbeType. All timings are int64
// microseconds with -1 meaning "not measured" (the wire convention).
package probes

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"syscall"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
)

// Prober runs one probe to completion. Implementations never return nil and
// never panic across the boundary intentionally; the scheduler additionally
// guards with recover.
type Prober interface {
	Run(ctx context.Context, spec *pb.ProbeSpec) *pb.ProbeResult
}

// Registry maps wire probe types to their implementations. A type absent
// from the registry is reported UNSUPPORTED by the scheduler, never skipped.
type Registry map[pb.ProbeType]Prober

// DefaultRegistry returns the probers this agent build supports.
func DefaultRegistry() Registry {
	return Registry{
		pb.ProbeType_PROBE_TYPE_ICMP:       NewICMP(),
		pb.ProbeType_PROBE_TYPE_TCP:        TCP{},
		pb.ProbeType_PROBE_TYPE_TLS:        TLS{},
		pb.ProbeType_PROBE_TYPE_HTTP:       HTTP{},
		pb.ProbeType_PROBE_TYPE_DNS:        DNS{},
		pb.ProbeType_PROBE_TYPE_NTP:        NTP{},
		pb.ProbeType_PROBE_TYPE_TRACEROUTE: Traceroute{},
		pb.ProbeType_PROBE_TYPE_PATH_MTU:   PathMTU{},
	}
}

// newResult returns a result stamped from the spec with every measurement
// marked "not measured" — probers fill in only what they actually measured.
func newResult(spec *pb.ProbeSpec, startedAt time.Time) *pb.ProbeResult {
	return &pb.ProbeResult{
		ProbeId:   spec.GetProbeId(),
		Type:      spec.GetType(),
		TargetId:  spec.GetTarget().GetTargetId(),
		StartedAt: timestamppb.New(startedAt),
		JitterUs:  -1,
		Rtt:       &pb.RttStats{MinUs: -1, AvgUs: -1, MaxUs: -1, StddevUs: -1},
		Timings:   &pb.Timings{DnsUs: -1, TcpConnectUs: -1, TlsHandshakeUs: -1, TtfbUs: -1, TotalUs: -1},
	}
}

// classify maps an error to a wire status. Order matters: timeouts win over
// everything (a timeout during TLS handshake is a TIMEOUT, not TLS_FAILURE).
func classify(err error) pb.ProbeStatus {
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return pb.ProbeStatus_PROBE_STATUS_TIMEOUT
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return pb.ProbeStatus_PROBE_STATUS_CONN_REFUSED
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return pb.ProbeStatus_PROBE_STATUS_DNS_FAILURE
	}
	if isTLSError(err) {
		return pb.ProbeStatus_PROBE_STATUS_TLS_FAILURE
	}
	return pb.ProbeStatus_PROBE_STATUS_ERROR
}

func isTLSError(err error) bool {
	var (
		certVerify   *tls.CertificateVerificationError
		recordHeader tls.RecordHeaderError
		unknownAuth  x509.UnknownAuthorityError
		hostname     x509.HostnameError
		certInvalid  x509.CertificateInvalidError
	)
	return errors.As(err, &certVerify) || errors.As(err, &recordHeader) ||
		errors.As(err, &unknownAuth) || errors.As(err, &hostname) || errors.As(err, &certInvalid)
}

func us(d time.Duration) int64 { return d.Microseconds() }

// fail finalizes a result for err: status classified (stageStatus wins when
// classify has nothing more specific than ERROR), message recorded.
func fail(res *pb.ProbeResult, err error, stageStatus pb.ProbeStatus) *pb.ProbeResult {
	st := classify(err)
	if st == pb.ProbeStatus_PROBE_STATUS_ERROR && stageStatus != pb.ProbeStatus_PROBE_STATUS_UNSPECIFIED {
		st = stageStatus
	}
	res.Status = st
	res.Error = err.Error()
	return res
}
