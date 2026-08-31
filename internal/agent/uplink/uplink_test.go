package uplink

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/devalexllc/polarbeam/internal/agent/config"
	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
	"github.com/devalexllc/polarbeam/internal/version"
)

// fakeRecv yields a scripted sequence of snapshots, then a terminal error.
type fakeRecv struct {
	snaps []*pb.ConfigSnapshot
	err   error
}

func (f *fakeRecv) Recv() (*pb.ConfigSnapshot, error) {
	if len(f.snaps) == 0 {
		return nil, f.err
	}
	s := f.snaps[0]
	f.snaps = f.snaps[1:]
	return s, nil
}

// fakeClock is a manually advanced clock for the now seam. Only Run's
// goroutine touches it in these tests (openStream fakes advance it), so the
// mutex guards nothing today but keeps it safe for reuse.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// sleepRecorder implements the after seam: it records every requested sleep
// and returns an already-closed channel so the sleep is instantaneous.
type sleepRecorder struct {
	mu    sync.Mutex
	slept []time.Duration
}

func (r *sleepRecorder) after(d time.Duration) <-chan time.Time {
	r.mu.Lock()
	r.slept = append(r.slept, d)
	r.mu.Unlock()
	ch := make(chan time.Time)
	close(ch)
	return ch
}

func (r *sleepRecorder) recorded() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Duration(nil), r.slept...)
}

func identityJitter(d time.Duration) time.Duration { return d }

// runUplink runs u.Run in a goroutine and returns a join func that fails the
// test if Run does not return promptly or returns a non-nil error.
func runUplink(t *testing.T, ctx context.Context, u *Uplink) (join func()) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- u.Run(ctx) }()
	return func() {
		t.Helper()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Run returned %v, want nil", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not return")
		}
	}
}

func TestRunBackoffDoublesAndCaps(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)} // frozen: every stream looks instant
	rec := &sleepRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	calls := 0
	u := &Uplink{
		now:      clk.now,
		after:    rec.after,
		jitterFn: identityJitter,
		openStream: func(context.Context, *pb.AgentHello) (configRecv, error) {
			calls++
			if calls > 9 {
				cancel() // stop the loop; Run must exit without another sleep
			}
			return nil, errors.New("dial refused")
		},
	}
	runUplink(t, ctx, u)()

	want := []time.Duration{
		1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 32 * time.Second, 60 * time.Second, 60 * time.Second,
		60 * time.Second,
	}
	got := rec.recorded()
	if len(got) != len(want) {
		t.Fatalf("recorded %d sleeps %v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sleep %d = %v, want %v (ladder %v)", i, got[i], want[i], got)
		}
	}
}

func TestRunHealthyStreamResetsBackoff(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	rec := &sleepRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	calls := 0
	u := &Uplink{
		now:      clk.now,
		after:    rec.after,
		jitterFn: identityJitter,
		openStream: func(context.Context, *pb.AgentHello) (configRecv, error) {
			calls++
			switch calls {
			case 3:
				// A stream that stayed up past healthyStream before failing.
				clk.advance(61 * time.Second)
			case 5:
				cancel()
			}
			return nil, errors.New("stream lost")
		},
	}
	runUplink(t, ctx, u)()

	// 1s, 2s escalate; the healthy stream resets to 1s; then 2s proves the
	// ladder restarted from the bottom rather than continuing at 4s.
	want := []time.Duration{1 * time.Second, 2 * time.Second, 1 * time.Second, 2 * time.Second}
	got := rec.recorded()
	if len(got) != len(want) {
		t.Fatalf("recorded sleeps %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sleep %d = %v, want %v (ladder %v)", i, got[i], want[i], got)
		}
	}
}

func TestRunHealthyStreamStillSleepsBeforeReconnect(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	rec := &sleepRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	calls := 0
	u := &Uplink{
		now:      clk.now,
		after:    rec.after,
		jitterFn: identityJitter,
		openStream: func(context.Context, *pb.AgentHello) (configRecv, error) {
			calls++
			switch calls {
			case 1:
				clk.advance(61 * time.Second) // healthy from the very first stream
			case 3:
				cancel()
			}
			return nil, errors.New("stream lost")
		},
	}
	runUplink(t, ctx, u)()

	// The reset happens before the sleep and the doubling after it: even a
	// healthy stream is followed by exactly one minimum-backoff sleep (never
	// zero), and the next fast failure sleeps 2s.
	want := []time.Duration{1 * time.Second, 2 * time.Second}
	got := rec.recorded()
	if len(got) != len(want) {
		t.Fatalf("recorded sleeps %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sleep %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestRunReturnsNilOnCtxCancelDuringStream(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	rec := &sleepRecorder{}
	ctx, cancel := context.WithCancel(context.Background())

	u := &Uplink{
		now:      clk.now,
		after:    rec.after,
		jitterFn: identityJitter,
		openStream: func(ctx context.Context, _ *pb.AgentHello) (configRecv, error) {
			cancel()
			return nil, ctx.Err()
		},
	}
	runUplink(t, ctx, u)()

	if got := rec.recorded(); len(got) != 0 {
		t.Errorf("Run slept %v after ctx cancel, want no sleeps", got)
	}
}

func TestRunReturnsNilOnCtxCancelDuringSleep(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sleeping := make(chan struct{})
	u := &Uplink{
		now:      clk.now,
		jitterFn: identityJitter,
		after: func(time.Duration) <-chan time.Time {
			close(sleeping)
			return make(chan time.Time) // never fires; only ctx can end the wait
		},
		openStream: func(context.Context, *pb.AgentHello) (configRecv, error) {
			return nil, errors.New("dial refused")
		},
	}
	join := runUplink(t, ctx, u)
	<-sleeping
	cancel()
	join()
}

func TestRunAppliesJitterToBackoff(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	rec := &sleepRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sentinel = 777 * time.Millisecond
	var jitterIn []time.Duration
	u := &Uplink{
		now:   clk.now,
		after: rec.after,
		jitterFn: func(d time.Duration) time.Duration {
			jitterIn = append(jitterIn, d)
			return sentinel
		},
		openStream: func(context.Context, *pb.AgentHello) (configRecv, error) {
			if len(jitterIn) > 0 {
				cancel()
			}
			return nil, errors.New("dial refused")
		},
	}
	runUplink(t, ctx, u)()

	if len(jitterIn) != 1 || jitterIn[0] != backoffMin {
		t.Errorf("jitter received %v, want [%v]", jitterIn, backoffMin)
	}
	if got := rec.recorded(); len(got) != 1 || got[0] != sentinel {
		t.Errorf("slept %v, want the jittered duration [%v]", got, sentinel)
	}
}

func TestStreamOnceDeliversSnapshotsInOrder(t *testing.T) {
	streamErr := errors.New("stream lost")
	var seen []string
	u := &Uplink{
		OnSnapshot: func(s *pb.ConfigSnapshot) { seen = append(seen, s.GetConfigHash()) },
		openStream: func(context.Context, *pb.AgentHello) (configRecv, error) {
			return &fakeRecv{
				snaps: []*pb.ConfigSnapshot{
					{ConfigHash: "h1"},
					{ConfigHash: "h2"},
				},
				err: streamErr,
			}, nil
		},
	}
	err := u.streamOnce(context.Background())
	if !errors.Is(err, streamErr) {
		t.Errorf("streamOnce = %v, want the Recv error", err)
	}
	if len(seen) != 2 || seen[0] != "h1" || seen[1] != "h2" {
		t.Errorf("OnSnapshot saw %v, want [h1 h2]", seen)
	}
	if u.configHash != "h2" {
		t.Errorf("configHash = %q, want the last snapshot's hash h2", u.configHash)
	}
}

func TestStreamOnceHelloCarriesCachedHashAndVersion(t *testing.T) {
	var hello *pb.AgentHello
	streamErr := errors.New("stream lost")
	u := &Uplink{
		openStream: func(_ context.Context, h *pb.AgentHello) (configRecv, error) {
			hello = h
			return &fakeRecv{err: streamErr}, nil
		},
	}
	u.SetCachedConfigHash("cached-hash")
	if err := u.streamOnce(context.Background()); !errors.Is(err, streamErr) {
		t.Fatalf("streamOnce = %v, want the Recv error", err)
	}
	if hello.GetConfigHash() != "cached-hash" {
		t.Errorf("hello.ConfigHash = %q, want the primed cached-hash", hello.GetConfigHash())
	}
	if hello.GetAgentVersion() != version.Version {
		t.Errorf("hello.AgentVersion = %q, want %q", hello.GetAgentVersion(), version.Version)
	}
}

func TestStreamOnceNilOnSnapshotDoesNotPanic(t *testing.T) {
	u := &Uplink{
		openStream: func(context.Context, *pb.AgentHello) (configRecv, error) {
			return &fakeRecv{
				snaps: []*pb.ConfigSnapshot{{ConfigHash: "h1"}},
				err:   errors.New("stream lost"),
			}, nil
		},
	}
	if err := u.streamOnce(context.Background()); err == nil {
		t.Error("streamOnce = nil, want the Recv error")
	}
	if u.configHash != "h1" {
		t.Errorf("configHash = %q, want h1 even without an OnSnapshot hook", u.configHash)
	}
}

func TestStreamOnceReturnsNilWhenCtxCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	u := &Uplink{
		openStream: func(context.Context, *pb.AgentHello) (configRecv, error) {
			cancel()
			return &fakeRecv{err: errors.New("transport closing")}, nil
		},
	}
	if err := u.streamOnce(ctx); err != nil {
		t.Errorf("streamOnce after ctx cancel = %v, want nil (clean shutdown)", err)
	}
}

func TestStreamOnceReturnsOpenError(t *testing.T) {
	openErr := errors.New("dial refused")
	called := false
	u := &Uplink{
		OnSnapshot: func(*pb.ConfigSnapshot) { called = true },
		openStream: func(context.Context, *pb.AgentHello) (configRecv, error) {
			return nil, openErr
		},
	}
	if err := u.streamOnce(context.Background()); !errors.Is(err, openErr) {
		t.Errorf("streamOnce = %v, want the open error", err)
	}
	if called {
		t.Error("OnSnapshot ran despite the stream never opening")
	}
}

func TestJitterBounds(t *testing.T) {
	for _, d := range []time.Duration{backoffMin, 10 * time.Second, backoffMax} {
		lo := time.Duration(0.8 * float64(d))
		hi := time.Duration(1.2 * float64(d))
		for i := 0; i < 10000; i++ {
			got := jitter(d)
			if got < lo-1 || got > hi+1 {
				t.Fatalf("jitter(%v) = %v, outside [%v, %v]", d, got, lo, hi)
			}
		}
	}
}

func TestNewDefaultsSeams(t *testing.T) {
	pki, _, _ := testPKI(t)
	cfg := config.Defaults()
	cfg.StateDir = filepath.Dir(pki.Dir)
	cfg.Server.Address = "127.0.0.1:1" // grpc.NewClient is lazy; never dialed
	u, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	if u.openStream == nil || u.now == nil || u.after == nil || u.jitterFn == nil {
		t.Error("New left a transport/clock seam nil; Run would panic")
	}
}
