package uplink

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/grpc/connectivity"
)

// watchdogTick is how often the watchdog samples the channel state.
const watchdogTick = 15 * time.Second

// ErrUnreachable is the cause the watchdog reports when the control plane
// has been unreachable for longer than server.unreachable_timeout while no
// probe succeeded either.
var ErrUnreachable = errors.New("control plane unreachable")

// NoteProbeOK records that a probe just produced an OK result. The
// scheduler sink calls it; the watchdog reads it to tell a dead container
// network (nothing works) from a control-plane outage (probes still work).
func (u *Uplink) NoteProbeOK() {
	u.lastProbeOK.Store(u.now().UnixNano())
}

func (u *Uplink) lastProbeOKAt() time.Time {
	ns := u.lastProbeOK.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// Watchdog exits the agent when its container network is dead: it returns
// an error wrapping ErrUnreachable once the gRPC channel has not been Ready
// for limit AND no probe has succeeded within that same window. It returns
// nil when ctx ends first. limit 0 disables it (returns nil immediately).
//
// Why exit instead of retrying harder: every layer already retries forever
// (config stream, pusher, gRPC's DNS resolver). The failure this covers is a
// container whose network was rebuilt underneath it — a Podman network reset
// leaves the running container's veth, routes and bind-mounted resolv.conf
// frozen, so no in-process retry can ever succeed. Only a container restart
// rebuilds the namespace, and the runtime's restart policy does that when
// the process exits. A restart is lossless: results wait in the disk spool
// and the cached config keeps probing running.
//
// Why probe success gates the exit: a restart only helps when the
// container's own network is broken, and a broken network fails every probe
// too. When probes still succeed the control plane itself is down, a restart
// cannot help, and restarting anyway would cancel long-interval probes before
// their first run on every cycle (the scheduler splays a probe's first run
// across its whole interval) — the agent must keep measuring and spooling
// through a control-plane outage. For the same reason the no-evidence
// budget is max(limit, 2 × the fastest schedulable probe's interval): the
// fastest probe has certainly run at least once within two of its intervals
// (splay ≤ one interval), so an agent whose only probes run hourly waits two
// hours, not ten minutes, before "no OK" means anything. An agent with no
// probes assigned has no such evidence and exits on unreachability alone,
// which starves nothing.
// Results carry no target address, so a probe against the container's own
// loopback (meaningless in production; Docker's embedded resolver in the
// dev stack) counts as evidence too — accepted rather than threading the
// spec through the sink.
//
// Channel state is the signal, not "last successful RPC": the config stream
// is always open, so the client keepalive (1 min ping, 20 s timeout in dial)
// closes a dead transport and the state leaves Ready within ~80 s, while an
// idle agent with no probes assigned (nothing to push, no snapshots for
// hours) still reads Ready. A reachable server that rejects RPCs stays Ready
// and never trips this.
func (u *Uplink) Watchdog(ctx context.Context, limit time.Duration) error {
	if limit <= 0 {
		return nil
	}
	// The grace period starts now: boot and every later outage get the same
	// budget measured from the last tick that saw the channel Ready.
	lastReady := u.now()
	heldBack := false
	fastest := func() time.Duration { return 0 }
	if u.FastestProbeInterval != nil {
		fastest = u.FastestProbeInterval
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-u.after(watchdogTick):
		}
		// A cancelled context must never surface as ErrUnreachable, even
		// when the timer was ready in the same select.
		if ctx.Err() != nil {
			return nil
		}
		now := u.now()
		if u.state() == connectivity.Ready {
			lastReady = now
			heldBack = false
			continue
		}
		since := now.Sub(lastReady)
		budget := max(limit, 2*fastest())
		if since < budget {
			continue
		}
		if ok := u.lastProbeOKAt(); !ok.IsZero() && now.Sub(ok) < budget {
			if !heldBack {
				heldBack = true
				slog.Warn("control plane unreachable but probes still succeed; not restarting — "+
					"control-plane outage, results are spooling",
					"unreachable_for", since.Truncate(time.Second), "last_probe_ok", now.Sub(ok).Truncate(time.Second),
					"limit", limit, "budget", budget, "key", "server.unreachable_timeout")
			}
			continue
		}
		slog.Error("control plane unreachable and no probe succeeded; exiting so the container runtime can restart the agent",
			"unreachable_for", since.Truncate(time.Second), "limit", limit, "budget", budget, "key", "server.unreachable_timeout")
		return fmt.Errorf("%w for %s and no probe succeeded (server.unreachable_timeout=%s, budget %s): restart the container to rebuild its network",
			ErrUnreachable, since.Truncate(time.Second), limit, budget)
	}
}
