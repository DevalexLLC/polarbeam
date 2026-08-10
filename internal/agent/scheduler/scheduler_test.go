package scheduler

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/devalexllc/polarbeam/internal/agent/probes"
	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
)

// countingProber counts runs per probe_id.
type countingProber struct {
	mu   sync.Mutex
	runs map[string]int
}

func (p *countingProber) Run(_ context.Context, spec *pb.ProbeSpec) *pb.ProbeResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.runs == nil {
		p.runs = make(map[string]int)
	}
	p.runs[spec.GetProbeId()]++
	return &pb.ProbeResult{
		ProbeId:  spec.GetProbeId(),
		Type:     spec.GetType(),
		TargetId: spec.GetTarget().GetTargetId(),
		Status:   pb.ProbeStatus_PROBE_STATUS_OK,
	}
}

func (p *countingProber) count(id string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runs[id]
}

func spec(id string, interval time.Duration) *pb.ProbeSpec {
	return &pb.ProbeSpec{
		ProbeId:  id,
		Type:     pb.ProbeType_PROBE_TYPE_TCP,
		Target:   &pb.Target{TargetId: "t-" + id},
		Interval: durationpb.New(interval),
		Timeout:  durationpb.New(interval / 2),
	}
}

func snapshot(specs ...*pb.ProbeSpec) *pb.ConfigSnapshot {
	return &pb.ConfigSnapshot{ConfigHash: "test", Probes: specs}
}

func collectSink() (func(*pb.ProbeResult), *atomic.Int64) {
	var n atomic.Int64
	return func(*pb.ProbeResult) { n.Add(1) }, &n
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}

func TestWorkersRunOnCadence(t *testing.T) {
	prober := &countingProber{}
	sink, sunk := collectSink()
	s := New(probes.Registry{pb.ProbeType_PROBE_TYPE_TCP: prober}, sink)
	defer s.Stop()

	s.Apply(snapshot(spec("a", 20*time.Millisecond), spec("b", 20*time.Millisecond)))
	waitFor(t, 2*time.Second, func() bool {
		return prober.count("a") >= 3 && prober.count("b") >= 3
	}, "workers did not run repeatedly")
	if sunk.Load() < 6 {
		t.Errorf("sink received %d results, want >= 6", sunk.Load())
	}
}

func TestApplyDiffSemantics(t *testing.T) {
	prober := &countingProber{}
	sink, _ := collectSink()
	s := New(probes.Registry{pb.ProbeType_PROBE_TYPE_TCP: prober}, sink)
	defer s.Stop()

	a := spec("a", 20*time.Millisecond)
	b := spec("b", 20*time.Millisecond)
	s.Apply(snapshot(a, b))
	waitFor(t, 2*time.Second, func() bool { return prober.count("a") >= 1 && prober.count("b") >= 1 },
		"initial workers did not run")

	// Remove b; a survives untouched and keeps running.
	aRunsBefore := prober.count("a")
	s.Apply(snapshot(a))
	waitFor(t, 2*time.Second, func() bool { return prober.count("a") > aRunsBefore },
		"unchanged worker was disturbed by reconcile")

	bRuns := prober.count("b")
	time.Sleep(100 * time.Millisecond)
	if got := prober.count("b"); got > bRuns+1 {
		t.Errorf("removed worker kept running: %d -> %d", bRuns, got)
	}

	// Changing a's interval must restart its worker (still runs after).
	a2 := spec("a", 10*time.Millisecond)
	s.Apply(snapshot(a2))
	restartRuns := prober.count("a")
	waitFor(t, 2*time.Second, func() bool { return prober.count("a") > restartRuns },
		"changed worker did not restart")
}

func TestUnsupportedTypeReported(t *testing.T) {
	var mu sync.Mutex
	var got []*pb.ProbeResult
	sink := func(r *pb.ProbeResult) {
		mu.Lock()
		got = append(got, r)
		mu.Unlock()
	}
	// Registry without DNS: a DNS spec must produce UNSUPPORTED results on
	// cadence, never be silently dropped.
	s := New(probes.Registry{}, sink)
	defer s.Stop()

	dns := spec("d", 20*time.Millisecond)
	dns.Type = pb.ProbeType_PROBE_TYPE_DNS
	s.Apply(snapshot(dns))

	waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) >= 2
	}, "UNSUPPORTED results were not emitted")
	mu.Lock()
	defer mu.Unlock()
	for _, r := range got {
		if r.Status != pb.ProbeStatus_PROBE_STATUS_UNSUPPORTED {
			t.Errorf("status = %v, want UNSUPPORTED", r.Status)
		}
		if r.Error == "" {
			t.Error("UNSUPPORTED result must explain itself")
		}
	}
}

type panicProber struct{}

func (panicProber) Run(context.Context, *pb.ProbeSpec) *pb.ProbeResult { panic("boom") }

func TestProberPanicBecomesErrorResult(t *testing.T) {
	var mu sync.Mutex
	var got []*pb.ProbeResult
	sink := func(r *pb.ProbeResult) {
		mu.Lock()
		got = append(got, r)
		mu.Unlock()
	}
	s := New(probes.Registry{pb.ProbeType_PROBE_TYPE_TCP: panicProber{}}, sink)
	defer s.Stop()

	s.Apply(snapshot(spec("p", 20*time.Millisecond)))
	waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) >= 2 // agent survived the first panic and ran again
	}, "panicking prober killed its worker")
	mu.Lock()
	defer mu.Unlock()
	if got[0].Status != pb.ProbeStatus_PROBE_STATUS_ERROR || got[0].Error == "" {
		t.Errorf("panic result = %v %q", got[0].Status, got[0].Error)
	}
}

// TestDuplicateProbeIDKeepsFirst: two specs sharing a probe_id must yield
// exactly one worker running the first spec — the duplicate is dropped
// loudly, never allowed to silently replace an earlier one.
func TestDuplicateProbeIDKeepsFirst(t *testing.T) {
	var mu sync.Mutex
	var got []*pb.ProbeResult
	sink := func(r *pb.ProbeResult) {
		mu.Lock()
		got = append(got, r)
		mu.Unlock()
	}
	s := New(probes.Registry{pb.ProbeType_PROBE_TYPE_TCP: &countingProber{}}, sink)
	defer s.Stop()

	first := spec("dup", 20*time.Millisecond)
	second := spec("dup", 20*time.Millisecond)
	second.Target = &pb.Target{TargetId: "t-other"}
	s.Apply(snapshot(first, second))

	s.mu.Lock()
	workers := len(s.workers)
	s.mu.Unlock()
	if workers != 1 {
		t.Fatalf("got %d workers, want 1", workers)
	}
	waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) >= 1
	}, "surviving worker never ran")
	mu.Lock()
	defer mu.Unlock()
	if got[0].GetTargetId() != first.GetTarget().GetTargetId() {
		t.Errorf("surviving worker runs target %q, want first spec's %q",
			got[0].GetTargetId(), first.GetTarget().GetTargetId())
	}
}

func TestStopTerminatesWorkers(t *testing.T) {
	prober := &countingProber{}
	sink, _ := collectSink()
	s := New(probes.Registry{pb.ProbeType_PROBE_TYPE_TCP: prober}, sink)
	s.Apply(snapshot(spec("a", 10*time.Millisecond)))
	waitFor(t, 2*time.Second, func() bool { return prober.count("a") >= 1 }, "worker never ran")

	s.Stop()
	runs := prober.count("a")
	time.Sleep(50 * time.Millisecond)
	if got := prober.count("a"); got != runs {
		t.Errorf("worker survived Stop: %d -> %d", runs, got)
	}
	// Apply after Stop must be a no-op, not a restart.
	s.Apply(snapshot(spec("z", 10*time.Millisecond)))
	time.Sleep(30 * time.Millisecond)
	if prober.count("z") != 0 {
		t.Error("Apply after Stop started a worker")
	}
}
