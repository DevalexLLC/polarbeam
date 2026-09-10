// polarbeam-agent is the PolarBEAM site agent: a single static binary that
// probes peers and endpoints, spools results to disk, and reports to the
// control plane over mTLS.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/devalexllc/polarbeam/internal/agent/confcache"
	"github.com/devalexllc/polarbeam/internal/agent/config"
	"github.com/devalexllc/polarbeam/internal/agent/enroll"
	"github.com/devalexllc/polarbeam/internal/agent/probes"
	"github.com/devalexllc/polarbeam/internal/agent/scheduler"
	"github.com/devalexllc/polarbeam/internal/agent/spool"
	"github.com/devalexllc/polarbeam/internal/agent/uplink"
	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
	"github.com/devalexllc/polarbeam/internal/retiredir"
	"github.com/devalexllc/polarbeam/internal/version"
)

const usage = `polarbeam-agent — PolarBEAM site agent

Usage:
  polarbeam-agent run       --config <file>         run the agent
  polarbeam-agent enroll    --config <file> --token <join-token>
                             (--ca-cert <file> | --fingerprint sha256:<hex>)
                             [--probe-address <host>]
                                                     enroll with the control plane
  polarbeam-agent selfcheck --config <file>         verify probe capabilities
                                                     (run performs the same
                                                     checks before starting)
  polarbeam-agent identity retire --config <file>   move <state_dir>/pki aside
                                                     (before re-enrolling)
  polarbeam-agent version                           print version and exit
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = cmdRun(os.Args[2:])
	case "enroll":
		err = cmdEnroll(os.Args[2:])
	case "selfcheck":
		err = cmdSelfcheck(os.Args[2:])
	case "identity":
		err = cmdIdentity(os.Args[2:])
	case "version", "--version":
		fmt.Println("polarbeam-agent", version.String())
		return
	case "help", "--help", "-h":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func loadConfig(fs *flag.FlagSet, args []string) (config.Config, error) {
	cfgPath := fs.String("config", "/etc/polarbeam/agent.yaml", "path to agent config file")
	if err := fs.Parse(args); err != nil {
		return config.Config{}, err
	}
	return config.Load(*cfgPath)
}

func cmdEnroll(args []string) error {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	token := fs.String("token", "", "one-time join token")
	caCert := fs.String("ca-cert", "", "path to the control plane CA certificate")
	fingerprint := fs.String("fingerprint", "", "pinned CA fingerprint (sha256:<hex>)")
	probeAddr := fs.String("probe-address", "", "address peers should probe (required behind NAT)")
	cfg, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	return enroll.Run(context.Background(), cfg, enroll.Options{
		Token:        *token,
		CACertFile:   *caCert,
		Fingerprint:  *fingerprint,
		ProbeAddress: *probeAddr,
	})
}

const identityUsage = "usage: polarbeam-agent identity retire --config <file>"

func cmdIdentity(args []string) error {
	if len(args) < 1 {
		return errors.New(identityUsage)
	}
	switch args[0] {
	case "retire":
		return cmdIdentityRetire(args[1:])
	}
	return errors.New(identityUsage)
}

// cmdIdentityRetire moves <state_dir>/pki aside so `enroll` can issue a
// replacement identity (it refuses while one exists). It replaces the
// former runbook's `rm -rf` through `--entrypoint sh`, which the
// shell-less release image cannot run. Rename only, never delete. The
// agent must be stopped first: the renewer writes into pki/ and there is
// no lock to detect a running instance.
func cmdIdentityRetire(args []string) error {
	fs := flag.NewFlagSet("identity retire", flag.ExitOnError)
	cfg, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	retired, err := retireIdentity(cfg.StateDir, time.Now())
	if err != nil {
		return err
	}
	fmt.Printf("identity retired: %s moved to %s\n"+
		"next: polarbeam-agent enroll --config <file> --token … issues the replacement identity\n"+
		"(same --probe-address as before); the spool is untouched and drains after re-enrollment.\n"+
		"Keep the retired directory until the new identity reports on the dashboard.\n",
		enroll.NewPKI(cfg.StateDir).Dir, retired)
	return nil
}

// retireIdentity renames <stateDir>/pki to a timestamped sibling and
// returns the new path.
func retireIdentity(stateDir string, now time.Time) (string, error) {
	retired, err := retiredir.Move(enroll.NewPKI(stateDir).Dir, now)
	if err != nil {
		return "", fmt.Errorf("identity retire: %w", err)
	}
	return retired, nil
}

// cmdSelfcheck verifies the capabilities the probers need (ICMP socket
// modes, traceroute's raw socket, spool writability) and exits non-zero if
// any fatal check fails. Config load itself is the first check: a bad file
// fails before anything else runs.
func cmdSelfcheck(args []string) error {
	fs := flag.NewFlagSet("selfcheck", flag.ExitOnError)
	cfg, err := loadConfig(fs, args)
	if err != nil {
		fmt.Printf(checkRow, "config", "FAIL", err)
		return errSelfcheck
	}
	return preflight(os.Stdout, cfg, fs.Lookup("config").Value.String())
}

// checkRow is the selfcheck output format: name, ok/FAIL, detail.
const checkRow = "%-18s %-5s %s\n"

var errSelfcheck = errors.New("selfcheck failed")

// preflight prints the config row and every selfcheck row for cfg and
// returns errSelfcheck when a fatal check failed. `run` calls it before
// starting so a misconfigured or capability-starved container fails loudly
// at start instead of degrading silently — the guarantee the former shell
// entrypoint wrapper (selfcheck && exec run) and, before it, the systemd
// ExecStartPre provided. There is deliberately no way to skip it.
func preflight(w io.Writer, cfg config.Config, cfgPath string) error {
	fmt.Fprintf(w, checkRow, "config", "ok", cfgPath)
	if !printChecks(w, probes.SelfCheck(cfg.StateDir)) {
		return errSelfcheck
	}
	return nil
}

// printChecks writes one row per check and reports whether every fatal
// check passed. Non-fatal failures print FAIL but do not block.
func printChecks(w io.Writer, checks []probes.Check) bool {
	ok := true
	for _, c := range checks {
		status := "ok"
		if !c.OK {
			status = "FAIL"
			if c.Fatal {
				ok = false
			}
		}
		fmt.Fprintf(w, checkRow, c.Name, status, c.Detail)
	}
	return ok
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfg, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	// Fail-loud preflight: rows go to stdout before logging is configured,
	// exactly as the former entrypoint wrapper printed them.
	if err := preflight(os.Stdout, cfg, fs.Lookup("config").Value.String()); err != nil {
		return fmt.Errorf("%w; not starting", err)
	}
	setupLogging(cfg.Log.Level)

	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// Cancellable in its own right: a fatal error (a spool write failure in
	// the scheduler sink) must also stop the background goroutines, or the
	// shutdown barrier below would wait forever. The cause distinguishes a
	// fatal from a clean signal-driven shutdown.
	ctx, cancel := context.WithCancelCause(sigCtx)
	defer cancel(nil)
	var wg sync.WaitGroup

	up, err := uplink.New(cfg)
	if err != nil {
		return err
	}
	defer up.Close()

	slog.Info("polarbeam-agent starting", "version", version.String(), "server", cfg.Server.Address,
		"unreachable_timeout", cfg.Server.UnreachableTimeout)

	// Spool-first single path: every result is written to disk, the pusher
	// drains from there. A spool that cannot be opened is fatal — running
	// without it would silently lose results across outages.
	sp, err := spool.Open(filepath.Join(cfg.StateDir, "spool"), cfg.Spool.MaxBytes, cfg.Spool.MaxAge)
	if err != nil {
		return err
	}
	defer sp.Close()

	// Every OK result is evidence the container's network works, which the
	// reachability watchdog needs to tell a dead network from a
	// control-plane outage (see uplink.Watchdog).
	sink := spoolSink(sp.Append, cancel)
	sched := scheduler.New(probes.DefaultRegistry(), func(res *pb.ProbeResult) {
		if res.GetStatus() == pb.ProbeStatus_PROBE_STATUS_OK {
			up.NoteProbeOK()
		}
		sink(res)
	})
	up.FastestProbeInterval = sched.FastestInterval
	defer sched.Stop()
	// The config cache is bound to the enrolled identity: re-enrollment
	// (wiping only pki/) must never leave the OLD agent's schedule to
	// drive the new one. uplink.New already proved we are enrolled, so an
	// unreadable identity here is a genuine error — run without a cache.
	agentID, err := enroll.NewPKI(cfg.StateDir).AgentID()
	if err != nil {
		slog.Error("agent identity unreadable; config cache disabled", "err", err)
	}
	// Apply first, persist second: the agent must run the new config even
	// when the cache write fails. A FAILED persist clears the cache — a
	// stale cache is worse than none, because the uplink already advertises
	// the newer hash on reconnect, so the server would never re-send and a
	// later restart would resurrect the old schedule.
	up.OnSnapshot = func(snap *pb.ConfigSnapshot) {
		sched.Apply(snap)
		if agentID == "" {
			return
		}
		if err := confcache.Store(cfg.StateDir, agentID, snap); err != nil {
			slog.Error("config cache write failed; clearing stale cache", "err", err)
			if err := confcache.Clear(cfg.StateDir, agentID); err != nil {
				slog.Error("config cache clear failed", "err", err)
			}
		}
	}
	// A cached snapshot keeps the agent measuring through a control-plane
	// outage that outlives a restart. The cache's hash rides AgentHello so
	// a reachable server that has moved on re-sends immediately.
	if agentID != "" {
		if cached, err := confcache.Load(cfg.StateDir, agentID); err != nil {
			slog.Error("config cache unreadable; waiting for the server", "err", err)
		} else if cached != nil {
			slog.Info("applying cached config snapshot",
				"hash", cached.GetConfigHash(), "probes", len(cached.GetProbes()))
			sched.Apply(cached)
			up.SetCachedConfigHash(cached.GetConfigHash())
		}
	}

	// Shutdown barrier. Declared last so it runs FIRST among the defers:
	// the pusher and the renewer are joined before sched.Stop, sp.Close and
	// up.Close tear down the spool and the gRPC connection underneath them.
	defer func() {
		cancel(nil)
		wg.Wait()
	}()

	pusher := uplink.NewPusher(up, sp)
	renewer := up.NewRenewer()
	wg.Add(3)
	go func() {
		defer wg.Done()
		pusher.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		renewer.Run(ctx)
	}()
	// Reachability watchdog: a channel that stays down past
	// server.unreachable_timeout with no probe succeeding either is fatal,
	// exactly like an unwritable spool — the exit hands recovery to the
	// container runtime's restart policy, which rebuilds the network
	// namespace no in-process retry can.
	go func() {
		defer wg.Done()
		if err := up.Watchdog(ctx, cfg.Server.UnreachableTimeout); err != nil {
			cancel(err)
		}
	}()
	runErr := up.Run(ctx)
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	return runErr
}

// spoolSink returns the scheduler sink: append every result to the spool, and
// on append failure cancel the run with the error as cause so the agent exits
// non-zero. The spool-first contract makes an unwritable spool fatal — running
// on would silently lose every result while the config stream keeps the agent
// looking online. Concurrency-safe as the scheduler requires; the first
// failure's cause wins.
func spoolSink(append func(*pb.ProbeResult) error, cancel context.CancelCauseFunc) func(*pb.ProbeResult) {
	return func(res *pb.ProbeResult) {
		if err := append(res); err != nil {
			slog.Error("spool append failed; terminating agent", "probe", res.GetProbeId(), "err", err)
			cancel(fmt.Errorf("spool append failed (probe %s): %w", res.GetProbeId(), err))
		}
	}
}

func setupLogging(level string) {
	var l slog.Level
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l})))
}
