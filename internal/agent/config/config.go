// Package config loads and validates the polarbeam-agent configuration.
// Loading is strict (unknown keys fatal); validation reports every problem
// with the offending key named. The agent refuses to start on any error.
package config

import (
	"errors"
	"fmt"
	"time"

	"github.com/devalexllc/polarbeam/internal/strictyaml"
)

type Config struct {
	Server   Server `yaml:"server"`
	StateDir string `yaml:"state_dir"`
	Spool    Spool  `yaml:"spool"`
	Log      Log    `yaml:"log"`
}

type Server struct {
	// Address is host:port of the control plane (normally port 443).
	Address string `yaml:"address"`
	// SNI overrides the TLS server name when it differs from the address
	// host (the proxy routes agents by SNI, e.g. grpc.polarbeam.example).
	SNI string `yaml:"sni"`
}

type Spool struct {
	// MaxBytes bounds spool disk usage; overflow drops oldest segments and
	// the drop is reported to the server (never silent).
	MaxBytes int64 `yaml:"max_bytes"`
	// MaxAge bounds how old spooled results may get before being dropped.
	MaxAge time.Duration `yaml:"max_age"`
}

type Log struct {
	Level string `yaml:"level"` // debug|info|warn|error
}

// Defaults returns the configuration defaults applied before file values.
func Defaults() Config {
	return Config{
		StateDir: "/var/lib/polarbeam-agent",
		Spool: Spool{
			MaxBytes: 256 << 20, // 256 MiB
			MaxAge:   7 * 24 * time.Hour,
		},
		Log: Log{Level: "info"},
	}
}

// Load reads path, applies defaults, and validates.
func Load(path string) (Config, error) {
	cfg := Defaults()
	if err := strictyaml.LoadFile(path, &cfg); err != nil {
		return Config{}, err
	}
	if err := cfg.validate(); err != nil {
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

func (c Config) validate() error {
	var errs []error
	if c.Server.Address == "" {
		errs = append(errs, errors.New("server.address is required"))
	}
	if c.StateDir == "" {
		errs = append(errs, errors.New("state_dir must not be empty"))
	}
	if c.Spool.MaxBytes <= 0 {
		errs = append(errs, errors.New("spool.max_bytes must be positive"))
	}
	if c.Spool.MaxAge <= 0 {
		errs = append(errs, errors.New("spool.max_age must be positive"))
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("log.level %q is not one of debug|info|warn|error", c.Log.Level))
	}
	return errors.Join(errs...)
}
