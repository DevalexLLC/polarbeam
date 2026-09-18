package store

import (
	"context"
	"fmt"
	"time"
)

// SyslogSettings is the single syslog_settings row: the audit-log
// forwarding destination edited from Settings -> Log forwarding.
// TLSClientKeyPEM is the stored private key; handlers must never echo it
// to clients.
type SyslogSettings struct {
	Enabled          bool
	Transport        string
	Host             string
	Port             int
	Framing          string
	Facility         int
	Hostname         string
	Content          string
	MinLevel         string
	OnFailure        string
	FailureTimeout   time.Duration
	TLSCAPEM         string
	TLSClientCertPEM string
	TLSClientKeyPEM  string
	TLSServerName    string
	UpdatedAt        time.Time
	UpdatedBy        string
}

const syslogColumns = `enabled, transport, host, port, framing, facility, hostname, content,
	min_level, on_failure, failure_timeout_ms, tls_ca_pem, tls_client_cert_pem,
	tls_client_key_pem, tls_server_name, updated_at, updated_by`

func scanSyslogSettings(row interface{ Scan(...any) error }) (*SyslogSettings, error) {
	var s SyslogSettings
	var timeoutMS int64
	var facility int16
	err := row.Scan(&s.Enabled, &s.Transport, &s.Host, &s.Port, &s.Framing, &facility, &s.Hostname, &s.Content,
		&s.MinLevel, &s.OnFailure, &timeoutMS, &s.TLSCAPEM, &s.TLSClientCertPEM,
		&s.TLSClientKeyPEM, &s.TLSServerName, &s.UpdatedAt, &s.UpdatedBy)
	if err != nil {
		return nil, err
	}
	s.Facility = int(facility)
	s.FailureTimeout = time.Duration(timeoutMS) * time.Millisecond
	return &s, nil
}

// GetSyslogSettings returns the forwarding configuration. The row is
// seeded by migration, so absence is a real error, not a default case.
func (s *Store) GetSyslogSettings(ctx context.Context) (*SyslogSettings, error) {
	out, err := scanSyslogSettings(s.pool.QueryRow(ctx, `SELECT `+syslogColumns+` FROM syslog_settings WHERE id`))
	if err != nil {
		return nil, fmt.Errorf("get syslog settings: %w", err)
	}
	return out, nil
}

// UpdateSyslogSettings replaces the configuration atomically and returns
// the stored row (updated_at from the DB clock). keepClientKey preserves
// the stored private key — the API's "empty means unchanged" convention,
// the client_secret pattern. The handler validates first; a CHECK firing
// here is a bug and stays loud.
func (s *Store) UpdateSyslogSettings(ctx context.Context, in SyslogSettings, keepClientKey bool) (*SyslogSettings, error) {
	out, err := scanSyslogSettings(s.pool.QueryRow(ctx, `
		UPDATE syslog_settings
		   SET enabled = $1, transport = $2, host = $3, port = $4, framing = $5,
		       facility = $6, hostname = $7, content = $8, min_level = $9,
		       on_failure = $10, failure_timeout_ms = $11, tls_ca_pem = $12,
		       tls_client_cert_pem = $13,
		       tls_client_key_pem = CASE WHEN $17 THEN tls_client_key_pem ELSE $14 END,
		       tls_server_name = $15, updated_at = now(), updated_by = $16
		 WHERE id
		 RETURNING `+syslogColumns,
		in.Enabled, in.Transport, in.Host, in.Port, in.Framing,
		int16(in.Facility), in.Hostname, in.Content, in.MinLevel,
		in.OnFailure, in.FailureTimeout.Milliseconds(), in.TLSCAPEM,
		in.TLSClientCertPEM, in.TLSClientKeyPEM, in.TLSServerName, in.UpdatedBy, keepClientKey))
	if err != nil {
		return nil, fmt.Errorf("update syslog settings: %w", err)
	}
	return out, nil
}
