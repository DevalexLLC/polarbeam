// Package uplink maintains the agent's mTLS gRPC channel to the control
// plane: the config stream (with reconnect/backoff) and, in later
// milestones, batched result pushes and certificate renewal.
package uplink

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	"github.com/devalexllc/polarbeam/internal/agent/config"
	"github.com/devalexllc/polarbeam/internal/agent/enroll"
	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
	"github.com/devalexllc/polarbeam/internal/version"
)

const (
	backoffMin = 1 * time.Second
	backoffMax = 60 * time.Second
)

// Uplink owns the gRPC connection and the config stream.
type Uplink struct {
	cfg config.Config
	pki enroll.PKI

	mu   sync.RWMutex
	conn *grpc.ClientConn

	// OnSnapshot is invoked for every received config snapshot (the
	// scheduler wires in here at M2).
	OnSnapshot func(*pb.ConfigSnapshot)

	configHash string
}

func New(cfg config.Config) (*Uplink, error) {
	pki := enroll.NewPKI(cfg.StateDir)
	if !pki.Enrolled() {
		return nil, fmt.Errorf("not enrolled: run `polarbeam-agent enroll` first (state dir %s)", cfg.StateDir)
	}
	conn, err := dial(cfg, pki)
	if err != nil {
		return nil, err
	}
	return &Uplink{cfg: cfg, pki: pki, conn: conn}, nil
}

func dial(cfg config.Config, pki enroll.PKI) (*grpc.ClientConn, error) {
	tlsCfg, err := pki.ClientTLS(cfg)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(cfg.Server.Address,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
		// The config stream is mostly idle; keepalive pings keep it from
		// being reaped by idle-timeout middleboxes (incl. our own proxy)
		// and detect dead paths. Must stay above the server's enforcement
		// minimum (30s).
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:    1 * time.Minute,
			Timeout: 20 * time.Second,
		}))
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", cfg.Server.Address, err)
	}
	return conn, nil
}

// SetCachedConfigHash primes the hash AgentHello reports when a locally
// cached snapshot was applied before connecting: a server whose current
// snapshot matches skips the redundant initial send, and any difference
// still triggers a full snapshot (the server always compares). Call before
// Run — configHash is otherwise touched only from Run's goroutine.
func (u *Uplink) SetCachedConfigHash(hash string) { u.configHash = hash }

func (u *Uplink) getConn() *grpc.ClientConn {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.conn
}

// Recycle replaces the gRPC connection so the next handshake presents the
// renewed certificate. Without it the old connection would live until its
// cert expires and then be dropped by the server's 30s revocation sweep — a
// visible outage instead of a seamless renewal. Closing the old connection
// errors the config stream and any in-flight push; both retry on the new
// connection via their existing backoff paths.
func (u *Uplink) Recycle() error {
	conn, err := dial(u.cfg, u.pki)
	if err != nil {
		return err
	}
	u.mu.Lock()
	old := u.conn
	u.conn = conn
	u.mu.Unlock()
	return old.Close()
}

func (u *Uplink) Close() error { return u.getConn().Close() }

// Run consumes the config stream until ctx is cancelled, reconnecting with
// jittered exponential backoff. A stream that stayed up for a while resets
// the backoff, so unrelated transient disconnects spread over days never
// accumulate into minute-long reconnect waits. Failures are logged, never
// silent.
func (u *Uplink) Run(ctx context.Context) error {
	const healthyStream = 60 * time.Second
	backoff := backoffMin
	for {
		start := time.Now()
		err := u.streamOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(start) > healthyStream {
			backoff = backoffMin
		}
		if err != nil {
			slog.Error("config stream failed", "err", err, "retry_in", backoff)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(jitter(backoff)):
		}
		backoff = min(backoff*2, backoffMax)
	}
}

func (u *Uplink) streamOnce(ctx context.Context) error {
	client := pb.NewAgentServiceClient(u.getConn())
	stream, err := client.StreamConfig(ctx, &pb.AgentHello{
		AgentVersion: version.Version,
		ConfigHash:   u.configHash,
	})
	if err != nil {
		return err
	}
	slog.Info("connected to control plane", "server", u.cfg.Server.Address)
	for {
		snap, err := stream.Recv()
		if err != nil {
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil
			}
			return err
		}
		slog.Info("config snapshot received",
			"hash", snap.GetConfigHash(), "probes", len(snap.GetProbes()))
		u.configHash = snap.GetConfigHash()
		if u.OnSnapshot != nil {
			u.OnSnapshot(snap)
		}
	}
}

func jitter(d time.Duration) time.Duration {
	// ±20% so a fleet that lost the server does not reconnect in lockstep.
	f := 0.8 + 0.4*rand.Float64()
	return time.Duration(float64(d) * f)
}
