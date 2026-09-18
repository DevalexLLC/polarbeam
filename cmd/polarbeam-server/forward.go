package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/devalexllc/polarbeam/internal/server"
	"github.com/devalexllc/polarbeam/internal/server/config"
	"github.com/devalexllc/polarbeam/internal/server/store"
	"github.com/devalexllc/polarbeam/internal/server/syslogfwd"
)

// installForwarder points this CLI process's logging at the stored syslog
// destination, if forwarding is enabled, and returns the function that
// drains it at exit. The CLI runs in its own process, so its audit records
// (cli.*) only reach the collector if it forwards them itself. Failure to
// set it up is reported on stderr and never fails the command: the record
// still lands on stderr.
func installForwarder(ctx context.Context, cfg config.Config, st *store.Store) func() {
	row, err := st.GetSyslogSettings(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "syslog forwarding unavailable for this command: %v\n", err)
		return func() {}
	}
	scfg := syslogfwd.FromSettings(row)
	if !scfg.Enabled {
		return func() {}
	}
	fwd := server.NewForwarder(cfg.Log.Level)
	// The tee goes in first: Apply's own start record is logged through the
	// default logger and must already reach the forwarder.
	server.SetupForwarding(cfg.Log.Level, fwd)
	if err := fwd.Apply(scfg, row.UpdatedAt); err != nil {
		server.SetupLogging(cfg.Log.Level)
		fmt.Fprintf(os.Stderr, "syslog forwarding unavailable for this command: %v\n", err)
		return func() {}
	}
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if undelivered := fwd.Close(ctx); undelivered > 0 {
			fmt.Fprintf(os.Stderr, "warning: %d audit record(s) not delivered to the syslog collector (see stderr above)\n", undelivered)
		}
	}
}

// bestEffortForwarder is installForwarder for subcommands that do not
// otherwise need the database (ca init/retire, tls install, migrate): a
// short connection attempt, and stderr-only logging when the database or
// the settings table is not there yet.
func bestEffortForwarder(cfg config.Config) func() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := store.Connect(ctx, cfg.DB.URL, 5*time.Second, 1)
	if err != nil {
		fmt.Fprintf(os.Stderr, "syslog forwarding unavailable for this command: %v\n", err)
		return func() {}
	}
	stop := installForwarder(ctx, cfg, st)
	st.Close()
	return stop
}
