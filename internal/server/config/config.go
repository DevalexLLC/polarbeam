// Package config loads and validates the polarbeam-server configuration.
// Loading is strict (unknown keys fatal) and validation is preflight-style:
// every problem is reported with the offending key named, and the server
// refuses to start on any error.
package config

import (
	"errors"
	"fmt"
	"time"

	"github.com/devalexllc/polarbeam/internal/strictyaml"
)

type Config struct {
	Listen Listen `yaml:"listen"`
	DB     DB     `yaml:"db"`
	TLS    TLS    `yaml:"tls"`
	CA     CA     `yaml:"ca"`
	Log    Log    `yaml:"log"`
}

type Listen struct {
	// GRPC is the agent-facing mTLS gRPC listener (behind the proxy's
	// SNI passthrough for grpc.<domain>).
	GRPC string `yaml:"grpc"`
	// GRPCHostname is the DNS name agents use to reach the gRPC listener
	// (their SNI). The built-in CA auto-issues the gRPC server certificate
	// with this SAN; the operator-provided tls.* certificate covers only
	// the dashboard listener.
	GRPCHostname string `yaml:"grpc_hostname"`
	// HTTP is the dashboard HTTPS listener.
	HTTP string `yaml:"http"`
}

type DB struct {
	// URL is a postgres connection string (postgres://user:pass@host:5432/db).
	URL string `yaml:"url"`
	// ConnectTimeout bounds startup preflight connection attempts.
	ConnectTimeout time.Duration `yaml:"connect_timeout"`
}

type TLS struct {
	// CertFile/KeyFile are the server certificate presented on both
	// listeners (dashboard HTTPS and gRPC server side of mTLS).
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

type CA struct {
	// Dir holds the built-in CA key and certificate (created by `ca init`).
	Dir string `yaml:"dir"`
	// AgentCertLifetime is the validity of issued agent client certificates.
	// Agents renew at 2/3 of the leaf's actual validity, so shortening this
	// (e.g. 10m) is the cert-rotation test mode; serve warns loudly below 24h.
	AgentCertLifetime time.Duration `yaml:"agent_cert_lifetime"`
	// ServerCertLifetime is the validity of the auto-issued gRPC server
	// certificate (reissued when less than 1/3 remains).
	ServerCertLifetime time.Duration `yaml:"server_cert_lifetime"`
}

type Log struct {
	Level string `yaml:"level"` // debug|info|warn|error
}

// Defaults returns the configuration defaults applied before file values.
func Defaults() Config {
	return Config{
		Listen: Listen{GRPC: ":8443", HTTP: ":8080"},
		DB:     DB{ConnectTimeout: 10 * time.Second},
		CA: CA{
			Dir:                "/var/lib/polarbeam-server/ca",
			AgentCertLifetime:  30 * 24 * time.Hour,
			ServerCertLifetime: 90 * 24 * time.Hour,
		},
		Log: Log{Level: "info"},
	}
}

// Load reads path, applies defaults, and validates. Any error is fatal to
// the caller by contract.
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
	if c.DB.URL == "" {
		errs = append(errs, errors.New("db.url is required"))
	}
	if c.TLS.CertFile == "" {
		errs = append(errs, errors.New("tls.cert_file is required"))
	}
	if c.TLS.KeyFile == "" {
		errs = append(errs, errors.New("tls.key_file is required"))
	}
	if c.Listen.GRPC == "" {
		errs = append(errs, errors.New("listen.grpc must not be empty"))
	}
	if c.Listen.GRPCHostname == "" {
		errs = append(errs, errors.New("listen.grpc_hostname is required (the DNS name agents connect to; SAN of the auto-issued gRPC certificate)"))
	}
	if c.Listen.HTTP == "" {
		errs = append(errs, errors.New("listen.http must not be empty"))
	}
	if c.CA.Dir == "" {
		errs = append(errs, errors.New("ca.dir must not be empty"))
	}
	// Issued certs are backdated 5 minutes (clock skew); below these floors
	// the renew-at-2/3 schedule degenerates into a renewal storm.
	if c.CA.AgentCertLifetime < 5*time.Minute {
		errs = append(errs, fmt.Errorf("ca.agent_cert_lifetime %s is below the 5m minimum", c.CA.AgentCertLifetime))
	}
	if c.CA.ServerCertLifetime < time.Hour {
		errs = append(errs, fmt.Errorf("ca.server_cert_lifetime %s is below the 1h minimum", c.CA.ServerCertLifetime))
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("log.level %q is not one of debug|info|warn|error", c.Log.Level))
	}
	return errors.Join(errs...)
}
