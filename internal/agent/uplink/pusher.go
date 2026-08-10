package uplink

import (
	"context"
	"log/slog"
	"time"

	"github.com/devalexllc/polarbeam/internal/agent/spool"
	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
)

const (
	batchSize   = 500
	batchWindow = 5 * time.Second
	pushTimeout = 30 * time.Second
)

// PushResults sends one batch over the shared mTLS channel, reporting the
// spool-loss counters alongside: the lifetime total (idempotent accounting on
// v0.4+ servers) and the unacked remainder (legacy delta for older servers).
func (u *Uplink) PushResults(ctx context.Context, results []*pb.ProbeResult, droppedTotal, droppedUnacked uint64) (uint32, error) {
	ctx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()
	resp, err := pb.NewAgentServiceClient(u.getConn()).PushResults(ctx, &pb.PushResultsRequest{
		Results:              results,
		DroppedSinceLastPush: droppedUnacked,
		// Always set, even at 0: presence marks a total-reporting agent.
		DroppedTotal: new(droppedTotal),
	})
	if err != nil {
		return 0, err
	}
	return resp.GetAccepted(), nil
}

// Pusher drains the spool to the control plane: batches of ≤500 results or a
// 5 s window, jittered exponential backoff on failure (1 s → 1 min), and a
// burst-drain after reconnect (back-to-back batches until the spool is
// empty). Results are acked — and only then deleted from the spool — after
// the server accepts them.
type Pusher struct {
	sp *spool.Spool
	// push is the transport seam (Uplink.PushResults in production,
	// stubbed in tests).
	push func(ctx context.Context, results []*pb.ProbeResult, droppedTotal, droppedUnacked uint64) (uint32, error)
}

func NewPusher(u *Uplink, sp *spool.Spool) *Pusher {
	return &Pusher{sp: sp, push: u.PushResults}
}

// Run drains until ctx is cancelled.
func (p *Pusher) Run(ctx context.Context) {
	backoff := backoffMin
	for ctx.Err() == nil {
		// Sleep until something is spooled.
		if p.sp.Pending() == 0 {
			select {
			case <-ctx.Done():
				return
			case <-p.sp.C():
			}
		}

		// Batching window: give results up to 5 s to accumulate, cut short
		// once a full batch is waiting.
		window := time.NewTimer(batchWindow)
	collect:
		for p.sp.Pending() < batchSize {
			select {
			case <-ctx.Done():
				window.Stop()
				return
			case <-window.C:
				break collect
			case <-p.sp.C():
			}
		}
		window.Stop()

		// Drain: back-to-back batches while the spool has data (burst after
		// an outage), pausing with backoff on push failure.
		for ctx.Err() == nil {
			results, ack := p.sp.Next(batchSize)
			if len(results) == 0 {
				break
			}
			total, unacked := p.sp.Dropped()
			accepted, err := p.push(ctx, results, total, unacked)
			if err != nil {
				slog.Error("result push failed", "results", len(results),
					"retry_in", backoff, "err", err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(jitter(backoff)):
				}
				backoff = min(backoff*2, backoffMax)
				break
			}
			backoff = backoffMin
			if unacked > 0 {
				p.sp.AckDropped(unacked)
			}
			if err := ack(); err != nil {
				slog.Error("spool ack failed", "err", err)
			}
			slog.Debug("results pushed", "sent", len(results), "accepted", accepted)
		}
	}
}
