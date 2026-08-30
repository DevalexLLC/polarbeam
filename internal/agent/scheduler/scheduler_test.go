package scheduler

import (
	"context"
	"fmt"
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
	// The prober's run count increments before its result reaches the sink,
	// so the sink may still be one delivery behind here.
	waitFor(t, 2*time.Second, func() bool {
		return sunk.Load() >= 6
	}, "sink did not receive 6 results")
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

// TestSplaySpreadsAcrossFullInterval: the first-run offset must cover the
// whole interval, not clump inside the first ~4.3s (the uint32-as-nanoseconds
// bug this guards against).
func TestSplaySpreadsAcrossFullInterval(t *testing.T) {
	interval := time.Minute
	var maxSplay time.Duration
	for i := 0; i < 256; i++ {
		sp := splayFor(fmt.Sprintf("probe-%d", i), interval)
		if sp < 0 || sp >= interval {
			t.Fatalf("splay %v outside [0, %v)", sp, interval)
		}
		if sp > maxSplay {
			maxSplay = sp
		}
	}
	if maxSplay <= 5*time.Second {
		t.Errorf("max splay over 256 probes = %v; offsets are clumped, not spread over the interval", maxSplay)
	}
}

// blockUntilCancel blocks until the run context ends, then reports status.
type blockUntilCancel struct {
	status  pb.ProbeStatus
	started chan struct{}
}

func (p *blockUntilCancel) Run(ctx context.Context, spec *pb.ProbeSpec) *pb.ProbeResult {
	select {
	case p.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return &pb.ProbeResult{ProbeId: spec.GetProbeId(), Status: p.status}
}

// TestCancelledRunsSuppressed: a failure produced because the worker was
// cancelled mid-run must not reach the sink (it would pollute loss
// accounting), while a measurement that still completed OK is kept.
func TestCancelledRunsSuppressed(t *testing.T) {
	for _, tc := range []struct {
		status pb.ProbeStatus
		want   int64
	}{
		{pb.ProbeStatus_PROBE_STATUS_TIMEOUT, 0},
		{pb.ProbeStatus_PROBE_STATUS_OK, 1},
	} {
		prober := &blockUntilCancel{status: tc.status, started: make(chan struct{}, 1)}
		sink, sunk := collectSink()
		s := New(probes.Registry{pb.ProbeType_PROBE_TYPE_TCP: prober}, sink)

		sp := spec("c", 20*time.Millisecond)
		sp.Timeout = durationpb.New(time.Minute) // only Stop ends the run
		s.Apply(snapshot(sp))
		<-prober.started
		s.Stop() // joins the worker before returning

		if got := sunk.Load(); got != tc.want {
			t.Errorf("status %v: sink received %d results, want %d", tc.status, got, tc.want)
		}
	}
}

// retireProber records Retire calls (probes.Retirer).
type retireProber struct {
	countingProber
	rmu     sync.Mutex
	retired []string
}

func (p *retireProber) Retire(id string) {
	p.rmu.Lock()
	defer p.rmu.Unlock()
	p.retired = append(p.retired, id)
}

func (p *retireProber) retiredIDs() []string {
	p.rmu.Lock()
	defer p.rmu.Unlock()
	return append([]string(nil), p.retired...)
}

// TestRetireCalledAfterWorkerExit: removing a probe must retire the prober's
// per-series state once its worker is gone.
func TestRetireCalledAfterWorkerExit(t *testing.T) {
	prober := &retireProber{}
	sink, _ := collectSink()
	s := New(probes.Registry{pb.ProbeType_PROBE_TYPE_TCP: prober}, sink)
	defer s.Stop()

	s.Apply(snapshot(spec("a", 10*time.Millisecond)))
	waitFor(t, 2*time.Second, func() bool { return prober.count("a") >= 1 }, "worker never ran")

	s.Apply(snapshot()) // remove a
	waitFor(t, 2*time.Second, func() bool {
		ids := prober.retiredIDs()
		return len(ids) == 1 && ids[0] == "a"
	}, "removed probe was not retired")
}

// blockingRetirer blocks in Run until cancelled and records Retire calls.
type blockingRetirer struct {
	blockUntilCancel
	rmu     sync.Mutex
	retired []string
}

func (p *blockingRetirer) Retire(id string) {
	p.rmu.Lock()
	defer p.rmu.Unlock()
	p.retired = append(p.retired, id)
}

func (p *blockingRetirer) retiredIDs() []string {
	p.rmu.Lock()
	defer p.rmu.Unlock()
	return append([]string(nil), p.retired...)
}

// TestRestartUnderSameIDDoesNotRetire: a changed spec restarts its worker
// under the same probe_id; the OLD worker's exit must not retire the state
// the replacement is already using. Retirement happens only when the id is
// truly gone.
func TestRestartUnderSameIDDoesNotRetire(t *testing.T) {
	prober := &blockingRetirer{blockUntilCancel: blockUntilCancel{
		status:  pb.ProbeStatus_PROBE_STATUS_OK,
		started: make(chan struct{}, 1),
	}}
	sink, _ := collectSink()
	s := New(probes.Registry{pb.ProbeType_PROBE_TYPE_TCP: prober}, sink)
	defer s.Stop()

	v1 := spec("x", 20*time.Millisecond)
	v1.Timeout = durationpb.New(time.Minute)
	s.Apply(snapshot(v1))
	<-prober.started

	v2 := spec("x", 30*time.Millisecond) // changed spec, same probe_id
	v2.Timeout = durationpb.New(time.Minute)
	s.Apply(snapshot(v2))
	<-prober.started // replacement worker is running

	time.Sleep(50 * time.Millisecond) // old worker has long exited
	if ids := prober.retiredIDs(); len(ids) != 0 {
		t.Fatalf("restart under same id retired state: %v", ids)
	}

	s.Apply(snapshot()) // now actually remove it
	waitFor(t, 2*time.Second, func() bool {
		ids := prober.retiredIDs()
		return len(ids) == 1 && ids[0] == "x"
	}, "removed probe was not retired")
}

// TestNonPositiveIntervalReportsError: a spec without a usable cadence must
// produce ERROR results on the fallback cadence, never go silent.
func TestNonPositiveIntervalReportsError(t *testing.T) {
	old := misconfiguredInterval
	misconfiguredInterval = 20 * time.Millisecond
	defer func() { misconfiguredInterval = old }()

	var mu sync.Mutex
	var got []*pb.ProbeResult
	sink := func(r *pb.ProbeResult) {
		mu.Lock()
		got = append(got, r)
		mu.Unlock()
	}
	s := New(probes.Registry{pb.ProbeType_PROBE_TYPE_TCP: &countingProber{}}, sink)
	defer s.Stop()

	s.Apply(snapshot(spec("bad", 0)))
	waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) >= 2
	}, "misconfigured spec produced no results")
	mu.Lock()
	defer mu.Unlock()
	for _, r := range got {
		if r.Status != pb.ProbeStatus_PROBE_STATUS_ERROR {
			t.Errorf("status = %v, want ERROR", r.Status)
		}
		if r.Error == "" {
			t.Error("misconfigured result must explain itself")
		}
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
