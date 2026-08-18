package grpcapi

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
	"github.com/devalexllc/polarbeam/internal/server/store"
	"github.com/devalexllc/polarbeam/internal/server/thresholds"
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

func TestResultToRowPathMtu(t *testing.T) {
	now := time.Now()
	base := func(status pb.ProbeStatus, m *pb.PathMtuResult) *pb.ProbeResult {
		r := validResult(now)
		r.Type = pb.ProbeType_PROBE_TYPE_PATH_MTU
		r.Status = status
		r.PathMtu = m
		return r
	}

	// A clean OK measurement maps fully and is usable.
	row, err := resultToRow(base(pb.ProbeStatus_PROBE_STATUS_OK, &pb.PathMtuResult{
		LargestOkBytes: 1400, SmallestFailedBytes: 1500, NextHopMtuBytes: 1400,
		IpVersion: 4, RttUs: 311,
	}), now)
	if err != nil {
		t.Fatalf("resultToRow: %v", err)
	}
	p := row.PathMtu
	if p == nil || p.LargestOK != 1400 || p.SmallestFailed != 1500 || p.NextHopMTU != 1400 ||
		p.IPVersion != 4 || p.RttUS == nil || *p.RttUS != 311 || !p.Usable {
		t.Errorf("payload = %+v, want mapped usable measurement", p)
	}

	// -1 RTT means not measured (NULL).
	row, err = resultToRow(base(pb.ProbeStatus_PROBE_STATUS_OK, &pb.PathMtuResult{
		LargestOkBytes: 1500, IpVersion: 6, RttUs: -1,
	}), now)
	if err != nil {
		t.Fatalf("resultToRow: %v", err)
	}
	if row.PathMtu.RttUS != nil || row.PathMtu.IPVersion != 6 {
		t.Errorf("payload = %+v, want nil rtt and ip_version 6", row.PathMtu)
	}

	// A black-hole TIMEOUT still brackets the MTU: usable.
	row, err = resultToRow(base(pb.ProbeStatus_PROBE_STATUS_TIMEOUT, &pb.PathMtuResult{
		LargestOkBytes: 1400, SmallestFailedBytes: 1401, IpVersion: 4,
		BlackHoleSuspected: true, RttUs: -1,
	}), now)
	if err != nil {
		t.Fatalf("resultToRow: %v", err)
	}
	if !row.PathMtu.Usable || !row.PathMtu.BlackHole {
		t.Errorf("black-hole payload = %+v, want usable with flag", row.PathMtu)
	}

	// A non-converged TIMEOUT (no black hole) and a run with no delivered
	// size must not fold into current/events.
	for name, m := range map[string]*pb.PathMtuResult{
		"partial timeout": {LargestOkBytes: 1280, IpVersion: 4, RttUs: -1},
		"nothing passed":  {LargestOkBytes: 0, SmallestFailedBytes: 1280, IpVersion: 4, RttUs: -1, BlackHoleSuspected: false},
	} {
		row, err := resultToRow(base(pb.ProbeStatus_PROBE_STATUS_TIMEOUT, m), now)
		if err != nil {
			t.Fatalf("%s: resultToRow: %v", name, err)
		}
		if row.PathMtu.Usable {
			t.Errorf("%s: payload must not be usable: %+v", name, row.PathMtu)
		}
	}
}

func TestResultToRowPathMtuRejects(t *testing.T) {
	now := time.Now()
	cases := map[string]*pb.PathMtuResult{
		"oversized largest_ok": {LargestOkBytes: 1 << 20, IpVersion: 4},
		"oversized next_hop":   {LargestOkBytes: 1400, NextHopMtuBytes: 1 << 20, IpVersion: 4},
		"bad ip_version":       {LargestOkBytes: 1400, IpVersion: 5},
		"missing ip_version":   {LargestOkBytes: 1400},
	}
	for name, m := range cases {
		r := validResult(now)
		r.Type = pb.ProbeType_PROBE_TYPE_PATH_MTU
		r.PathMtu = m
		if _, err := resultToRow(r, now); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
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

func TestToOutageResultsGrading(t *testing.T) {
	probeID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	crit := thresholds.T{LatencyWarnUS: 10_000, LatencyCritUS: 40_000, LossWarnPct: 1, LossCritPct: 5}
	assigned := map[uuid.UUID]probeAssignment{probeID: {TargetID: uuid.NameSpaceURL, Crit: crit}}
	i32 := func(v int32) *int32 { return &v }
	f32 := func(v float32) *float32 { return &v }
	okStatus := int16(pb.ProbeStatus_PROBE_STATUS_OK)
	base := store.ResultRow{ProbeID: probeID, TargetID: uuid.NameSpaceURL, Time: time.Unix(100, 0)}

	cases := []struct {
		name     string
		row      store.ResultRow
		degraded bool
	}{
		{"clean ok", func() store.ResultRow {
			r := base
			r.Status = okStatus
			r.RttAvgUS = i32(5_000)
			return r
		}(), false},
		{"latency breach", func() store.ResultRow {
			r := base
			r.Status = okStatus
			r.RttAvgUS = i32(40_000)
			return r
		}(), true},
		// The latency ladder mirrors the read side: rtt_avg beats total.
		{"rtt beats total", func() store.ResultRow {
			r := base
			r.Status = okStatus
			r.RttAvgUS = i32(5_000)
			r.TotalUS = i32(90_000)
			return r
		}(), false},
		{"total when no rtt", func() store.ResultRow {
			r := base
			r.Status = okStatus
			r.TotalUS = i32(90_000)
			return r
		}(), true},
		{"loss breach", func() store.ResultRow {
			r := base
			r.Status = okStatus
			r.LossPct = f32(5)
			return r
		}(), true},
		// Failures are never graded, whatever their metrics say.
		{"failure ungraded", func() store.ResultRow {
			r := base
			r.Status = int16(pb.ProbeStatus_PROBE_STATUS_TIMEOUT)
			r.RttAvgUS = i32(999_999)
			return r
		}(), false},
	}
	for _, tc := range cases {
		out := toOutageResults([]store.ResultRow{tc.row}, assigned)
		if len(out) != 1 || out[0].Degraded != tc.degraded {
			t.Errorf("%s: degraded = %v, want %v", tc.name, out[0].Degraded, tc.degraded)
		}
		if out[0].Degraded && out[0].DegradedDetail == "" {
			t.Errorf("%s: breaching result must carry a detail", tc.name)
		}
	}
	// A row without an assignment entry (cannot happen post-filter, but the
	// map is authoritative) grades as not degraded rather than panicking.
	orphan := base
	orphan.Status = okStatus
	orphan.ProbeID = uuid.NameSpaceOID
	orphan.RttAvgUS = i32(999_999)
	if out := toOutageResults([]store.ResultRow{orphan}, assigned); out[0].Degraded {
		t.Error("unassigned row must not grade degraded")
	}
}
