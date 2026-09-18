package syslogfwd

import (
	"time"

	"github.com/devalexllc/polarbeam/internal/server/store"
)

// FromSettings maps the stored row onto a Config.
func FromSettings(s *store.SyslogSettings) Config {
	return Config{
		Enabled: s.Enabled, Transport: s.Transport, Host: s.Host, Port: s.Port,
		Framing: s.Framing, Facility: s.Facility, Hostname: s.Hostname,
		Content: s.Content, MinLevel: s.MinLevel, OnFailure: s.OnFailure,
		FailureTimeout: s.FailureTimeout,
		CAPEM:          s.TLSCAPEM, ClientCertPEM: s.TLSClientCertPEM, ClientKeyPEM: s.TLSClientKeyPEM,
		ServerName: s.TLSServerName,
	}
}

// Defaults is the migration-seeded row as a Config (disabled, TLS on 6514,
// local0, everything, info, warn after 5 m).
func Defaults() Config {
	return Config{
		Transport: TransportTLS, Port: DefaultPort, Framing: FramingOctetCounted,
		Facility: DefaultFacility, Content: ContentAll, MinLevel: "info",
		OnFailure: OnFailureWarn, FailureTimeout: DefaultFailureTimeout,
	}
}

var _ = time.Second
