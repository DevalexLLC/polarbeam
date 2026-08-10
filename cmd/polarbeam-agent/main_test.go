package main

import (
	"context"
	"errors"
	"strings"
	"testing"

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
