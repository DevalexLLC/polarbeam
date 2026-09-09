package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/devalexllc/polarbeam/internal/agent/config"
	"github.com/devalexllc/polarbeam/internal/agent/probes"
	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
)

func TestSpoolSinkFatalOnAppendError(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	boom := errors.New("write: no space left on device")
	sink := spoolSink(func(*pb.ProbeResult) error { return boom }, cancel)

	sink(&pb.ProbeResult{ProbeId: "probe-1"})
	if ctx.Err() == nil {
		t.Fatal("append failure must cancel the run context")
	}
	cause := context.Cause(ctx)
	if !errors.Is(cause, boom) {
		t.Fatalf("cause = %v, want it to wrap %v", cause, boom)
	}
	if !strings.Contains(cause.Error(), "probe-1") {
		t.Errorf("cause %q does not name the failing probe", cause)
	}

	// The first failure's cause wins; later failures must not replace it.
	sink(&pb.ProbeResult{ProbeId: "probe-2"})
	if got := context.Cause(ctx); !strings.Contains(got.Error(), "probe-1") {
		t.Errorf("cause replaced by a later failure: %v", got)
	}
}

func TestSpoolSinkNoCancelOnSuccess(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	sink := spoolSink(func(*pb.ProbeResult) error { return nil }, cancel)
	sink(&pb.ProbeResult{ProbeId: "probe-1"})
	if ctx.Err() != nil {
		t.Fatal("successful append must not cancel the run context")
	}
}

func TestPrintChecks(t *testing.T) {
	t.Run("fatal failure blocks", func(t *testing.T) {
		var buf bytes.Buffer
		ok := printChecks(&buf, []probes.Check{
			{Name: "icmp", OK: true, Detail: "echo probing available"},
			{Name: "spool", OK: false, Fatal: true, Detail: "cannot write"},
		})
		if ok {
			t.Fatal("a failed fatal check must report not ok")
		}
		out := buf.String()
		if !strings.HasPrefix(out, "icmp               ok    echo probing available\n") {
			t.Errorf("first row layout wrong:\n%s", out)
		}
		if !strings.Contains(out, "spool              FAIL  cannot write\n") {
			t.Errorf("fatal row missing or misformatted:\n%s", out)
		}
	})
	t.Run("non-fatal failure passes", func(t *testing.T) {
		var buf bytes.Buffer
		ok := printChecks(&buf, []probes.Check{
			{Name: "icmp6", OK: false, Fatal: false, Detail: "no ICMPv6"},
		})
		if !ok {
			t.Fatal("a non-fatal failure must not block")
		}
		if !strings.Contains(buf.String(), "icmp6              FAIL  no ICMPv6\n") {
			t.Errorf("non-fatal row must still print FAIL:\n%s", buf.String())
		}
	})
}

// TestPreflightFatalSpoolBlocksRun pins the contract the former shell
// entrypoint provided: a fatal selfcheck failure returns an error before
// run can start, and the offending row is printed.
func TestPreflightFatalSpoolBlocksRun(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory modes")
	}
	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(stateDir, 0o700) })
	cfg := config.Defaults()
	cfg.StateDir = stateDir

	var buf bytes.Buffer
	err := preflight(&buf, cfg, "/etc/polarbeam/agent.yaml")
	if !errors.Is(err, errSelfcheck) {
		t.Fatalf("preflight err = %v, want errSelfcheck", err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "config             ok    /etc/polarbeam/agent.yaml\n") {
		t.Errorf("config row missing:\n%s", out)
	}
	if !strings.Contains(out, "spool              FAIL  cannot create "+stateDir) {
		t.Errorf("fatal spool row missing:\n%s", out)
	}
}
