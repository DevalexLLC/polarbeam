package server

import (
	"log/slog"
	"os"

	"github.com/devalexllc/polarbeam/internal/server/syslogfwd"
	"github.com/devalexllc/polarbeam/internal/version"
)

// stderrHandler is the process's local log: text on standard error at the
// configured level, what `docker compose logs` shows.
func stderrHandler(level string) slog.Handler {
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
	return slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l})
}

// SetupLogging installs the stderr handler as the default logger. Every
// subcommand calls it right after loading its configuration.
func SetupLogging(level string) {
	slog.SetDefault(slog.New(stderrHandler(level)))
}

// NewForwarder builds the syslog forwarder for this process. Its alerts
// go to the stderr handler alone — never through itself.
func NewForwarder(level string) *syslogfwd.Forwarder {
	return syslogfwd.New(syslogfwd.Options{Version: version.String(), Alert: stderrHandler(level)})
}

// SetupForwarding installs stderr + forwarder as the default logger: from
// here on every record the process logs is offered to both.
func SetupForwarding(level string, fwd *syslogfwd.Forwarder) {
	slog.SetDefault(slog.New(syslogfwd.Tee(stderrHandler(level), fwd)))
}
