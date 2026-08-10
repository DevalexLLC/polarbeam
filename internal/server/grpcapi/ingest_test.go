package grpcapi

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
)

func validResult(now time.Time) *pb.ProbeResult {
	return &pb.ProbeResult{
		ProbeId:   uuid.NewString(),
		Type:      pb.ProbeType_PROBE_TYPE_TCP,
		TargetId:  uuid.NewString(),
		StartedAt: timestamppb.New(now.Add(-time.Second)),
		Status:    pb.ProbeStatus_PROBE_STATUS_OK,
		Sent:      1,
		Received:  1,
		JitterUs:  -1,
		Timings: &pb.Timings{
			DnsUs: -1, TcpConnectUs: 1234, TlsHandshakeUs: -1, TtfbUs: -1, TotalUs: 1234,
		},
	}
}

func TestResultToRowMapsWireValues(t *testing.T) {
	now := time.Now()
	r := validResult(now)
	row, err := resultToRow(r, now)
	if err != nil {
		t.Fatalf("resultToRow: %v", err)
	}
	if row.ProbeType != int16(pb.ProbeType_PROBE_TYPE_TCP) || row.Status != int16(pb.ProbeStatus_PROBE_STATUS_OK) {
		t.Errorf("type/status = %d/%d", row.ProbeType, row.Status)
	}
	if row.TCPConnectUS == nil || *row.TCPConnectUS != 1234 {
		t.Errorf("tcp_connect_us = %v, want 1234", row.TCPConnectUS)
	}
	// -1 on the wire means "not measured" and must land as NULL.
	if row.DNSUS != nil || row.TLSHandshakeUS != nil || row.TTFBUS != nil || row.JitterUS != nil {
		t.Errorf("unmeasured timings must be nil: dns=%v tls=%v ttfb=%v jitter=%v",
			row.DNSUS, row.TLSHandshakeUS, row.TTFBUS, row.JitterUS)
	}
	if row.RttMinUS != nil {
		t.Errorf("absent rtt stats must be nil, got %v", row.RttMinUS)
	}
	if row.LossPct == nil || *row.LossPct != 0 {
		t.Errorf("loss_pct = %v, want 0", row.LossPct)
	}
	if row.Error != nil {
		t.Errorf("error must be nil on success, got %q", *row.Error)
	}
}

func TestResultToRowLoss(t *testing.T) {
	now := time.Now()
	r := validResult(now)
	r.Sent, r.Received = 10, 7
	row, err := resultToRow(r, now)
	if err != nil {
		t.Fatalf("resultToRow: %v", err)
	}
	if row.LossPct == nil || *row.LossPct < 29.9 || *row.LossPct > 30.1 {
		t.Errorf("loss_pct = %v, want ~30", row.LossPct)
	}

	r.Sent, r.Received = 0, 0
	row, err = resultToRow(r, now)
	if err != nil {
		t.Fatalf("resultToRow: %v", err)
	}
	if row.LossPct != nil {
		t.Errorf("loss_pct with sent=0 = %v, want nil", *row.LossPct)
	}
}

func TestResultToRowTruncatesError(t *testing.T) {
	now := time.Now()
	r := validResult(now)
	r.Status = pb.ProbeStatus_PROBE_STATUS_ERROR
	r.Error = strings.Repeat("x", 500)
	row, err := resultToRow(r, now)
	if err != nil {
		t.Fatalf("resultToRow: %v", err)
	}
	if row.Error == nil || len(*row.Error) != maxErrorLen {
		t.Errorf("error length = %v, want %d", row.Error, maxErrorLen)
	}
}

func TestResultToRowRejects(t *testing.T) {
	now := time.Now()
	cases := map[string]func(*pb.ProbeResult){
		"bad probe_id":      func(r *pb.ProbeResult) { r.ProbeId = "nope" },
		"bad target_id":     func(r *pb.ProbeResult) { r.TargetId = "" },
		"missing timestamp": func(r *pb.ProbeResult) { r.StartedAt = nil },
		"future timestamp":  func(r *pb.ProbeResult) { r.StartedAt = timestamppb.New(now.Add(time.Hour)) },
		"no status":         func(r *pb.ProbeResult) { r.Status = pb.ProbeStatus_PROBE_STATUS_UNSPECIFIED },
	}
	for name, mutate := range cases {
		r := validResult(now)
		mutate(r)
		if _, err := resultToRow(r, now); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

func TestUsColumnClamps(t *testing.T) {
	if usColumn(-1) != nil {
		t.Error("-1 must map to nil")
	}
	if v := usColumn(1 << 40); v == nil || *v != 1<<31-1 {
		t.Errorf("overflow must clamp to max int32, got %v", v)
	}
}

func TestResultToRowTraceroute(t *testing.T) {
	now := time.Now()
	r := validResult(now)
	r.Type = pb.ProbeType_PROBE_TYPE_TRACEROUTE
	r.Traceroute = &pb.TracerouteResult{
		Hops: []*pb.Hop{
			{Ttl: 1, Addrs: []string{"10.0.0.1"}, RttUs: []int64{311}},
			{Ttl: 2}, // silent hop
		},
		DestReached: true,
		PathHash:    bytes.Repeat([]byte{0xab}, 32),
	}
	row, err := resultToRow(r, now)
	if err != nil {
		t.Fatalf("resultToRow: %v", err)
	}
	if row.Traceroute == nil || !row.Traceroute.DestReached {
		t.Fatalf("traceroute payload missing: %+v", row.Traceroute)
	}
	want := `[{"ttl":1,"addrs":["10.0.0.1"],"rtt_us":[311]},{"ttl":2,"addrs":[],"rtt_us":[]}]`
	if string(row.Traceroute.Hops) != want {
		t.Errorf("hops json = %s, want %s", row.Traceroute.Hops, want)
	}
}

func TestResultToRowTracerouteRejectsBadHash(t *testing.T) {
	now := time.Now()
	r := validResult(now)
	r.Type = pb.ProbeType_PROBE_TYPE_TRACEROUTE
	r.Traceroute = &pb.TracerouteResult{DestReached: true, PathHash: []byte{1, 2, 3}}
	if _, err := resultToRow(r, now); err == nil {
		t.Error("complete traceroute with short path_hash must be rejected")
	}
}
