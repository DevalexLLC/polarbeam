// polarbeam-agent is the PolarBEAM site agent: a single static binary that
// probes peers and endpoints, spools results to disk, and reports to the
// control plane over mTLS.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/devalexllc/polarbeam/internal/agent/config"
	"github.com/devalexllc/polarbeam/internal/agent/enroll"
	"github.com/devalexllc/polarbeam/internal/agent/probes"
	"github.com/devalexllc/polarbeam/internal/agent/scheduler"
	"github.com/devalexllc/polarbeam/internal/agent/spool"
	"github.com/devalexllc/polarbeam/internal/agent/uplink"
	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
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

// cmdSelfcheck verifies the capabilities the probers need (ICMP socket
// modes, traceroute's raw socket, spool writability) and exits non-zero if
// any fatal check fails. Config load itself is the first check: a bad file
// fails before anything else runs.
func cmdSelfcheck(args []string) error {
	fs := flag.NewFlagSet("selfcheck", flag.ExitOnError)
	cfg, err := loadConfig(fs, args)
	if err != nil {
		fmt.Printf("%-18s %-5s %v\n", "config", "FAIL", err)
		return fmt.Errorf("selfcheck failed")
	}
	fmt.Printf("%-18s %-5s %s\n", "config", "ok", fs.Lookup("config").Value.String())

	failed := false
	for _, c := range probes.SelfCheck(cfg.StateDir) {
		status := "ok"
		if !c.OK {
			status = "FAIL"
			if c.Fatal {
				failed = true
			}
		}
		fmt.Printf("%-18s %-5s %s\n", c.Name, status, c.Detail)
	}
	if failed {
		return fmt.Errorf("selfcheck failed")
	}
	return nil
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfg, err := loadConfig(fs, args)
	if err != nil {
		return err
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

	slog.Info("polarbeam-agent starting", "version", version.String(), "server", cfg.Server.Address)

	// Spool-first single path: every result is written to disk, the pusher
	// drains from there. A spool that cannot be opened is fatal — running
	// without it would silently lose results across outages.
	sp, err := spool.Open(filepath.Join(cfg.StateDir, "spool"), cfg.Spool.MaxBytes, cfg.Spool.MaxAge)
	if err != nil {
		return err
	}
	defer sp.Close()

	sched := scheduler.New(probes.DefaultRegistry(), spoolSink(sp.Append, cancel))
	defer sched.Stop()
	up.OnSnapshot = sched.Apply

	// Shutdown barrier. Declared last so it runs FIRST among the defers:
	// the pusher and the renewer are joined before sched.Stop, sp.Close and
	// up.Close tear down the spool and the gRPC connection underneath them.
	defer func() {
		cancel(nil)
		wg.Wait()
	}()

	pusher := uplink.NewPusher(up, sp)
	renewer := up.NewRenewer()
	wg.Add(2)
	go func() {
		defer wg.Done()
		pusher.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		renewer.Run(ctx)
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
