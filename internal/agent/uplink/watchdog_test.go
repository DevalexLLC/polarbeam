package uplink

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/connectivity"
)

// scriptedState feeds Watchdog one connectivity state per tick and advances
// the fake clock by one tick per sample, so elapsed time in the test is
// exactly ticks × watchdogTick. The last state repeats once the script runs
// out; onExhausted (if set) fires the first time that happens.
type scriptedState struct {
	mu          sync.Mutex
	clk         *fakeClock
	states      []connectivity.State
	onExhausted func()
	samples     int
}

func (s *scriptedState) state() connectivity.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.samples++
	if len(s.states) > 1 {
		st := s.states[0]
		s.states = s.states[1:]
		return st
	}
	if s.onExhausted != nil {
		s.onExhausted()
		s.onExhausted = nil
	}
	return s.states[0]
}

func (s *scriptedState) sampled() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.samples
}

// tickingAfter is the after seam for these tests: every sleep advances the
// fake clock by the requested duration and returns immediately.
func tickingAfter(clk *fakeClock) func(time.Duration) <-chan time.Time {
	return func(d time.Duration) <-chan time.Time {
		clk.advance(d)
		ch := make(chan time.Time)
		close(ch)
		return ch
	}
}

func repeat(st connectivity.State, n int) []connectivity.State {
	out := make([]connectivity.State, n)
	for i := range out {
		out[i] = st
	}
	return out
}

func runWatchdog(t *testing.T, ctx context.Context, u *Uplink, limit time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- u.Watchdog(ctx, limit) }()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Watchdog did not return")
		return nil
	}
}

func TestWatchdogZeroLimitDisables(t *testing.T) {
	rec := &sleepRecorder{}
	u := &Uplink{now: time.Now, after: rec.after, state: func() connectivity.State {
		t.Fatal("state sampled with the watchdog disabled")
		return connectivity.Ready
	}}
	if err := runWatchdog(t, context.Background(), u, 0); err != nil {
		t.Fatalf("disabled watchdog returned %v", err)
	}
	if got := rec.recorded(); len(got) != 0 {
		t.Fatalf("disabled watchdog slept: %v", got)
	}
}

func TestWatchdogReadyNeverTrips(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 100 ticks = 25 min of Ready against a 2 m limit, then cancel.
	script := &scriptedState{clk: clk, states: repeat(connectivity.Ready, 100), onExhausted: cancel}
	u := &Uplink{now: clk.now, after: tickingAfter(clk), state: script.state}
	if err := runWatchdog(t, ctx, u, 2*time.Minute); err != nil {
		t.Fatalf("Ready channel tripped the watchdog: %v", err)
	}
	if script.sampled() < 100 {
		t.Fatalf("watchdog stopped sampling early: %d samples", script.sampled())
	}
}

func TestWatchdogTripsAfterLimit(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	// limit 10 m = 40 ticks of 15 s: the 40th sample sees elapsed == limit
	// and must trip.
	script := &scriptedState{clk: clk, states: repeat(connectivity.TransientFailure, 200)}
	u := &Uplink{now: clk.now, after: tickingAfter(clk), state: script.state}
	err := runWatchdog(t, context.Background(), u, 10*time.Minute)
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
	if got, want := script.sampled(), 40; got != want {
		t.Fatalf("tripped after %d samples, want %d (elapsed == limit)", got, want)
	}
	for _, s := range []string{"10m0s", "server.unreachable_timeout=10m0s", "restart the container"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error %q lacks %q", err, s)
		}
	}
}

func TestWatchdogReadyResetsBudget(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 9 m down (36 ticks), one Ready tick, 9 m down again: never trips at a
	// 10 m limit. Then cancel.
	states := append(repeat(connectivity.Idle, 36), connectivity.Ready)
	states = append(states, repeat(connectivity.Connecting, 36)...)
	script := &scriptedState{clk: clk, states: states, onExhausted: cancel}
	u := &Uplink{now: clk.now, after: tickingAfter(clk), state: script.state}
	if err := runWatchdog(t, ctx, u, 10*time.Minute); err != nil {
		t.Fatalf("watchdog tripped across a Ready reset: %v", err)
	}
}

// TestWatchdogProbeSuccessHoldsBack: a control-plane-only outage (channel
// down, probes still OK) must not restart the agent — it would cancel
// long-interval probes before their first run on every cycle.
func TestWatchdogProbeSuccessHoldsBack(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 200 ticks = 50 min down at a 10 m limit; a probe succeeds every tick.
	script := &scriptedState{clk: clk, states: repeat(connectivity.TransientFailure, 200), onExhausted: cancel}
	u := &Uplink{now: clk.now, state: script.state}
	u.after = func(d time.Duration) <-chan time.Time {
		u.NoteProbeOK() // the scheduler sink, firing between ticks
		return tickingAfter(clk)(d)
	}
	if err := runWatchdog(t, ctx, u, 10*time.Minute); err != nil {
		t.Fatalf("watchdog tripped while probes succeeded: %v", err)
	}
	if script.sampled() < 200 {
		t.Fatalf("watchdog stopped sampling early: %d samples", script.sampled())
	}
}

// TestWatchdogStaleProbeSuccessTrips: probe successes older than the limit
// are not evidence the network works now.
func TestWatchdogStaleProbeSuccessTrips(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	script := &scriptedState{clk: clk, states: repeat(connectivity.TransientFailure, 200)}
	u := &Uplink{now: clk.now, after: tickingAfter(clk), state: script.state}
	u.NoteProbeOK() // at t=0, the moment the fault hits
	err := runWatchdog(t, context.Background(), u, 10*time.Minute)
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
	// Channel budget expires at tick 40 (10 m); the probe evidence is 10 m
	// old at the same tick, so it trips there, not later.
	if got, want := script.sampled(), 40; got != want {
		t.Fatalf("tripped after %d samples, want %d", got, want)
	}
}

// TestWatchdogCancelRacesTimer: cancellation with a ready timer and an
// exhausted budget must still return nil — Go's select picks randomly
// between ready cases, so the loop re-checks ctx after every tick.
func TestWatchdogCancelRacesTimer(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already done before the first select
	u := &Uplink{now: clk.now, after: tickingAfter(clk),
		state: func() connectivity.State { return connectivity.TransientFailure }}
	for range 50 {
		if err := runWatchdog(t, ctx, u, 15*time.Second); err != nil {
			t.Fatalf("cancelled watchdog returned %v", err)
		}
	}
}

// TestWatchdogBudgetStretchesToFastestProbe: an agent whose only probes run
// hourly cannot have produced an OK result within a 10 m window, so the
// no-evidence budget must stretch to two intervals — otherwise every
// control-plane outage restarts it and the hourly probes' splayed first run
// never arrives.
func TestWatchdogBudgetStretchesToFastestProbe(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	script := &scriptedState{clk: clk, states: repeat(connectivity.TransientFailure, 2000)}
	u := &Uplink{now: clk.now, after: tickingAfter(clk), state: script.state,
		FastestProbeInterval: func() time.Duration { return time.Hour }}
	err := runWatchdog(t, context.Background(), u, 10*time.Minute)
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
	// 2 h budget = 480 ticks; the 10 m limit alone would be 40.
	if got, want := script.sampled(), 480; got != want {
		t.Fatalf("tripped after %d samples, want %d (2 × fastest interval)", got, want)
	}
}

// TestWatchdogFastProbeKeepsLimit: a 10 s probe proves the network within
// seconds, so the budget stays at the configured limit.
func TestWatchdogFastProbeKeepsLimit(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	script := &scriptedState{clk: clk, states: repeat(connectivity.TransientFailure, 200)}
	u := &Uplink{now: clk.now, after: tickingAfter(clk), state: script.state,
		FastestProbeInterval: func() time.Duration { return 10 * time.Second }}
	err := runWatchdog(t, context.Background(), u, 10*time.Minute)
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
	if got, want := script.sampled(), 40; got != want {
		t.Fatalf("tripped after %d samples, want %d", got, want)
	}
}

// TestWatchdogHourlyProbeSuccessHoldsBack: one OK from an hourly probe is
// evidence for two hours, long enough for its next run to renew it.
func TestWatchdogHourlyProbeSuccessHoldsBack(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 1000 ticks ≈ 4 h down; the hourly probe succeeds every 240 ticks.
	script := &scriptedState{clk: clk, states: repeat(connectivity.TransientFailure, 1000), onExhausted: cancel}
	u := &Uplink{now: clk.now, state: script.state,
		FastestProbeInterval: func() time.Duration { return time.Hour }}
	ticks := 0
	u.after = func(d time.Duration) <-chan time.Time {
		if ticks%240 == 0 {
			u.NoteProbeOK()
		}
		ticks++
		return tickingAfter(clk)(d)
	}
	if err := runWatchdog(t, ctx, u, 10*time.Minute); err != nil {
		t.Fatalf("watchdog tripped while the hourly probe kept succeeding: %v", err)
	}
}

func TestWatchdogCancelReturnsNil(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	u := &Uplink{now: time.Now, after: func(time.Duration) <-chan time.Time { return make(chan time.Time) },
		state: func() connectivity.State { return connectivity.TransientFailure }}
	if err := runWatchdog(t, ctx, u, 2*time.Minute); err != nil {
		t.Fatalf("cancelled watchdog returned %v", err)
	}
}
