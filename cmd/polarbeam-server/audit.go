package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/devalexllc/polarbeam/internal/audit"
)

// auditLog is the CLI's audit sink. Every subcommand that changes state
// records what it did (cli.* events) with the invoking OS user as the
// actor, so privileged operator actions taken at the container CLI land
// in the same trail as dashboard writes (ASD STIG V-222463/512).
var auditLog = audit.New(nil)

// cliActor is who ran the command: $USER, or "unknown" in a container
// that does not set it (the distroless image runs as uid 10001 with no
// passwd entry).
func cliActor() audit.Actor {
	user := os.Getenv("USER")
	if user == "" {
		user = "unknown"
	}
	return audit.Actor{User: user, Source: audit.SourceCLI}
}

// auditCLI records one completed CLI action.
func auditCLI(id, msg string, outcome audit.Outcome, attrs ...slog.Attr) {
	auditLog.Emit(context.Background(), audit.Event{
		ID: id, Msg: msg, Outcome: outcome, Actor: cliActor(), Attrs: attrs,
	})
}
