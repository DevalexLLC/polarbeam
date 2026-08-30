package probes

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
)

func TestRttStats(t *testing.T) {
	cases := []struct {
		rtts                  []int64
		min, avg, max, stddev int64
	}{
		{[]int64{100}, 100, 100, 100, 0},
		{[]int64{100, 200}, 100, 150, 200, 50},
		{[]int64{100, 100, 100}, 100, 100, 100, 0},
		{[]int64{50, 150, 250}, 50, 150, 250, 82}, // population stddev ≈ 81.65
	}
	for _, c := range cases {
		min, avg, max, stddev := rttStats(c.rtts)
		if min != c.min || avg != c.avg || max != c.max || stddev != c.stddev {
			t.Errorf("rttStats(%v) = %d/%d/%d/%d, want %d/%d/%d/%d",
				c.rtts, min, avg, max, stddev, c.min, c.avg, c.max, c.stddev)
		}
	}
}

func TestFoldJitterAcrossRuns(t *testing.T) {
	p := NewICMP()

	// No RTTs, then a single RTT: never two consecutive samples → -1.
	if j := p.foldJitter("a", nil); j != -1 {
		t.Errorf("empty run: jitter = %d, want -1", j)
	}
	if j := p.foldJitter("a", []int64{100}); j != -1 {
		t.Errorf("first sample: jitter = %d, want -1", j)
	}
	// Second run folds against the carried last RTT: J = (16-0)/16 = 1.
	if j := p.foldJitter("a", []int64{116}); j != 1 {
		t.Errorf("first fold: jitter = %d, want 1", j)
	}
	// |116-116| = 0 → J = 1 + (0-1)/16 = 0.9375, reported rounded to 1.
	if j := p.foldJitter("a", []int64{116}); j != 1 {
		t.Errorf("decay fold: jitter = %d, want 1", j)
	}
	// A distinct series has independent state.
	if j := p.foldJitter("b", []int64{100}); j != -1 {
		t.Errorf("other series primed with -1 expected, got %d", j)
	}
}

// TestRetireDropsJitterState: a retired series must not resume its stale
// accumulator if the probe is later re-assigned.
func TestRetireDropsJitterState(t *testing.T) {
	p := NewICMP()
	p.foldJitter("x", []int64{100})
	if j := p.foldJitter("x", []int64{116}); j == -1 {
		t.Fatal("accumulator never primed")
	}
	p.Retire("x")
	if j := p.foldJitter("x", []int64{100}); j != -1 {
		t.Errorf("jitter after retire = %d, want -1 (fresh accumulator)", j)
	}
}

func icmpSpec(address string, count int, spacing time.Duration) *pb.ProbeSpec {
	return &pb.ProbeSpec{
		ProbeId: "test-icmp",
		Type:    pb.ProbeType_PROBE_TYPE_ICMP,
		Target: &pb.Target{
			Kind:     pb.TargetKind_TARGET_KIND_EXTERNAL,
			TargetId: "test-target",
			Address:  address,
		},
		Interval:     durationpb.New(time.Second),
		Timeout:      durationpb.New(3 * time.Second),
		TrainCount:   uint32(count),
		TrainSpacing: durationpb.New(spacing),
	}
}

func TestICMPLoopback(t *testing.T) {
	if _, _, err := openICMP(net.ParseIP("127.0.0.1")); err != nil {
		t.Skipf("no ICMP socket available (needs ping_group_range or CAP_NET_RAW): %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	res := NewICMP().Run(ctx, icmpSpec("127.0.0.1", 3, 10*time.Millisecond))
	if res.Status != pb.ProbeStatus_PROBE_STATUS_OK {
		t.Fatalf("status = %v, error = %q", res.Status, res.Error)
	}
	if res.Sent != 3 || res.Received != 3 {
		t.Errorf("sent/received = %d/%d, want 3/3", res.Sent, res.Received)
	}
	if res.Rtt.MinUs < 0 || res.Rtt.AvgUs < 0 || res.Rtt.MaxUs < 0 || res.Rtt.StddevUs < 0 {
		t.Errorf("rtt stats not measured: %+v", res.Rtt)
	}
	if res.Rtt.MinUs > res.Rtt.AvgUs || res.Rtt.AvgUs > res.Rtt.MaxUs {
		t.Errorf("rtt ordering violated: %+v", res.Rtt)
	}
	// Echo trains measure RTT, not connection phases.
	if res.Timings.TcpConnectUs != -1 || res.Timings.TotalUs != -1 {
		t.Errorf("timings must stay unmeasured: %+v", res.Timings)
	}
}

func TestICMPResolutionFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res := NewICMP().Run(ctx, icmpSpec("host.invalid", 1, time.Millisecond))
	if res.Status != pb.ProbeStatus_PROBE_STATUS_DNS_FAILURE {
		t.Errorf("status = %v (error %q), want DNS_FAILURE", res.Status, res.Error)
	}
}
