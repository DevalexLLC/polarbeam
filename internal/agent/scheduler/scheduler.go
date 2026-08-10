// Package scheduler runs one worker per assigned ProbeSpec and reconciles
// the worker set against incoming config snapshots: the server always sends
// FULL snapshots, and the agent diffs locally (by probe_id + spec hash) so
// unchanged probes keep their cadence across config pushes.
package scheduler

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/devalexllc/polarbeam/internal/agent/probes"
	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
)

// Scheduler owns the probe workers. Apply is safe for concurrent use with
// itself and Stop (the uplink calls it from its stream goroutine).
type Scheduler struct {
	registry probes.Registry
	sink     func(*pb.ProbeResult)

	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	workers map[string]*worker // keyed by probe_id
	wg      sync.WaitGroup
}

type worker struct {
	specHash string
	cancel   context.CancelFunc
}

// New creates a scheduler delivering every completed result to sink (called
// from worker goroutines; the sink must be concurrency-safe).
func New(registry probes.Registry, sink func(*pb.ProbeResult)) *Scheduler {
	ctx, cancel := context.WithCancel(context.Background())
	return &Scheduler{
		registry: registry,
		sink:     sink,
		ctx:      ctx,
		cancel:   cancel,
		workers:  make(map[string]*worker),
	}
}

// Apply reconciles the worker set with a full snapshot: removed probes stop,
// changed probes restart, unchanged probes are untouched.
func (s *Scheduler) Apply(snap *pb.ConfigSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return
	}

	desired := make(map[string]*pb.ProbeSpec, len(snap.GetProbes()))
	for _, spec := range snap.GetProbes() {
		if spec.GetProbeId() == "" {
			slog.Error("ignoring probe spec without probe_id")
			continue
		}
		if _, dup := desired[spec.GetProbeId()]; dup {
			// Fail loud, never silently replace: a duplicate would collapse
			// two probes onto one worker. Server-side derivation makes this
			// unreachable; keeping the first is deterministic because specs
			// arrive sorted by probe_id.
			slog.Error("duplicate probe_id in snapshot; keeping first",
				"probe", spec.GetProbeId())
			continue
		}
		desired[spec.GetProbeId()] = spec
	}

	for id, w := range s.workers {
		spec, keep := desired[id]
		if keep && w.specHash == specHash(spec) {
			delete(desired, id) // unchanged: leave running
			continue
		}
		w.cancel()
		delete(s.workers, id)
	}

	for id, spec := range desired {
		s.startWorkerLocked(id, spec)
	}
	slog.Info("probe schedule reconciled", "workers", len(s.workers))
}

// Stop cancels all workers and waits for them to finish.
func (s *Scheduler) Stop() {
	s.cancel()
	s.mu.Lock()
	for id, w := range s.workers {
		w.cancel()
		delete(s.workers, id)
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Scheduler) startWorkerLocked(id string, spec *pb.ProbeSpec) {
	interval := spec.GetInterval().AsDuration()
	if interval <= 0 {
		// Fail loud, don't run: a probe without a cadence cannot be
		// scheduled. Server-side validation makes this unreachable in
		// practice.
		slog.Error("ignoring probe spec with non-positive interval",
			"probe", id, "interval", interval)
		return
	}

	prober, ok := s.registry[spec.GetType()]
	if !ok {
		// Reported on the normal cadence as UNSUPPORTED — never silently
		// skipped. The server surfaces it as a config problem.
		slog.Error("unsupported probe type: reporting UNSUPPORTED",
			"probe", id, "type", spec.GetType())
		prober = unsupported{}
	}

	ctx, cancel := context.WithCancel(s.ctx)
	s.workers[id] = &worker{specHash: specHash(spec), cancel: cancel}
	s.wg.Add(1)
	go s.runWorker(ctx, spec, prober)
}

func (s *Scheduler) runWorker(ctx context.Context, spec *pb.ProbeSpec, prober probes.Prober) {
	defer s.wg.Done()
	interval := spec.GetInterval().AsDuration()

	// Splay: hash(probe_id) % interval offsets first runs so a site's
	// probes don't fire in lockstep after a config push or restart.
	h := fnv.New32a()
	h.Write([]byte(spec.GetProbeId()))
	splay := time.Duration(h.Sum32()) % interval
	select {
	case <-ctx.Done():
		return
	case <-time.After(splay):
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		s.sink(s.runOnce(ctx, spec, prober))
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// runOnce executes a single probe run under the spec's timeout, converting a
// prober panic into an ERROR result so one bad prober cannot kill the agent.
func (s *Scheduler) runOnce(ctx context.Context, spec *pb.ProbeSpec, prober probes.Prober) (res *pb.ProbeResult) {
	timeout := spec.GetTimeout().AsDuration()
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	started := time.Now()
	defer func() {
		if r := recover(); r != nil {
			slog.Error("prober panicked", "probe", spec.GetProbeId(), "panic", r)
			res = &pb.ProbeResult{
				ProbeId:   spec.GetProbeId(),
				Type:      spec.GetType(),
				TargetId:  spec.GetTarget().GetTargetId(),
				StartedAt: timestamppb.New(started),
				Status:    pb.ProbeStatus_PROBE_STATUS_ERROR,
				Error:     fmt.Sprintf("prober panic: %v", r),
				JitterUs:  -1,
			}
		}
	}()
	return prober.Run(runCtx, spec)
}

// unsupported reports UNSUPPORTED for probe types this agent cannot run.
type unsupported struct{}

func (unsupported) Run(_ context.Context, spec *pb.ProbeSpec) *pb.ProbeResult {
	return &pb.ProbeResult{
		ProbeId:   spec.GetProbeId(),
		Type:      spec.GetType(),
		TargetId:  spec.GetTarget().GetTargetId(),
		StartedAt: timestamppb.New(time.Now()),
		Status:    pb.ProbeStatus_PROBE_STATUS_UNSUPPORTED,
		Error:     fmt.Sprintf("probe type %s not supported by this agent", spec.GetType()),
		JitterUs:  -1,
	}
}

// specHash fingerprints a spec so Apply can tell changed from unchanged.
func specHash(spec *pb.ProbeSpec) string {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(spec)
	if err != nil {
		// Cannot happen for a well-formed in-memory message; force restart.
		return fmt.Sprintf("marshal-error-%p", spec)
	}
	h := fnv.New64a()
	h.Write(b)
	return fmt.Sprintf("%x", h.Sum64())
}
