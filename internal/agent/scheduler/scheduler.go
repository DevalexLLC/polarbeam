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
			// Cannot be reported on the wire: results are keyed by probe_id,
			// so there is no series to attribute an ERROR to. Local log is
			// the loudest available channel.
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

// misconfiguredInterval is the fallback cadence for specs whose own interval
// is unusable. A var so tests can shorten it.
var misconfiguredInterval = time.Minute

func (s *Scheduler) startWorkerLocked(id string, spec *pb.ProbeSpec) {
	interval := spec.GetInterval().AsDuration()

	prober, ok := s.registry[spec.GetType()]
	if !ok {
		// Reported on the normal cadence as UNSUPPORTED — never silently
		// skipped. The server surfaces it as a config problem.
		slog.Error("unsupported probe type: reporting UNSUPPORTED",
			"probe", id, "type", spec.GetType())
		prober = unsupported{}
	}

	if interval <= 0 {
		// Fail loud on the wire, not just locally: a probe without a usable
		// cadence is reported as ERROR on a fallback cadence so the server
		// sees the config problem instead of the series going silent.
		// Server-side validation makes this unreachable in practice.
		slog.Error("probe spec has non-positive interval: reporting ERROR",
			"probe", id, "interval", interval)
		prober = misconfigured{reason: fmt.Sprintf("non-positive interval %v", interval)}
		interval = misconfiguredInterval
	}

	ctx, cancel := context.WithCancel(s.ctx)
	s.workers[id] = &worker{specHash: specHash(spec), cancel: cancel}
	s.wg.Add(1)
	go s.runWorker(ctx, spec, prober, interval)
}

// retire drops per-series prober state after a worker exits — unless a
// replacement worker already owns the probe_id (a changed spec restarted it
// under the same ID before the old goroutine finished): retiring then would
// delete the replacement's live state midstream.
func (s *Scheduler) retire(id string, r probes.Retirer) {
	s.mu.Lock()
	_, replaced := s.workers[id]
	s.mu.Unlock()
	if !replaced {
		r.Retire(id)
	}
}

func (s *Scheduler) runWorker(ctx context.Context, spec *pb.ProbeSpec, prober probes.Prober, interval time.Duration) {
	defer s.wg.Done()
	// Deferred so it runs after any in-flight run of THIS worker has folded.
	if r, ok := prober.(probes.Retirer); ok {
		defer s.retire(spec.GetProbeId(), r)
	}

	select {
	case <-ctx.Done():
		return
	case <-time.After(splayFor(spec.GetProbeId(), interval)):
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		res := s.runOnce(ctx, spec, prober)
		// A run cut short because the worker was cancelled (shutdown or a
		// config change) yields a failure that reflects the cancellation,
		// not the path — spooling it would pollute loss accounting. A
		// measurement that completed OK despite racing the cancel is real
		// and is kept.
		if ctx.Err() == nil || res.GetStatus() == pb.ProbeStatus_PROBE_STATUS_OK {
			s.sink(res)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// splayFor offsets a probe's first run so a site's probes don't fire in
// lockstep after a config push or restart. A 64-bit hash spreads the offset
// across the FULL interval; a 32-bit hash read as nanoseconds would cap the
// spread at ~4.3s and re-clump every longer cadence.
func splayFor(probeID string, interval time.Duration) time.Duration {
	h := fnv.New64a()
	h.Write([]byte(probeID))
	return time.Duration(h.Sum64() % uint64(interval))
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

// misconfigured reports ERROR for specs the agent cannot schedule as sent
// (same fail-loud shape as unsupported, with the defect in the message).
type misconfigured struct{ reason string }

func (m misconfigured) Run(_ context.Context, spec *pb.ProbeSpec) *pb.ProbeResult {
	return &pb.ProbeResult{
		ProbeId:   spec.GetProbeId(),
		Type:      spec.GetType(),
		TargetId:  spec.GetTarget().GetTargetId(),
		StartedAt: timestamppb.New(time.Now()),
		Status:    pb.ProbeStatus_PROBE_STATUS_ERROR,
		Error:     fmt.Sprintf("misconfigured probe spec: %s", m.reason),
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
