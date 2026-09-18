// Package syslogfwd forwards the server's log records — every audit record
// and, optionally, the operational ones — to a syslog collector as RFC
// 5424 messages over TLS (RFC 5425), TCP (RFC 6587), or UDP (RFC 5426).
//
// It is a slog.Handler: installed beside the stderr handler through Tee,
// it sees every record the server logs, classifies audit records by the
// reserved event attribute (internal/audit), formats each as one syslog
// message, and hands it to a bounded queue drained by a writer goroutine
// that owns the connection. The queue is the buffer that keeps records
// through a collector outage; the writer's failure handling is what gives
// the operator the AU-5 guarantees: an immediate alert on stderr, a
// warning at 75 % of the buffer, an accounted drop of the oldest records
// when it fills, a recovery record carrying the drop count, and — when
// the policy is halt — a fatal signal after the configured timeout.
//
// No dependency beyond the standard library: the built-in log/syslog is
// RFC 3164 only and has no TLS.
package syslogfwd

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Transports, framings, and policies (the syslog_settings enums).
const (
	TransportUDP = "udp"
	TransportTCP = "tcp"
	TransportTLS = "tls"

	FramingOctetCounted   = "octet-counted"   // RFC 6587 §3.4.1 / RFC 5425 §4.3
	FramingNonTransparent = "non-transparent" // RFC 6587 §3.4.2, LF trailer

	ContentAll   = "all"   // audit records plus operational records at MinLevel and above
	ContentAudit = "audit" // audit records only

	OnFailureWarn = "warn" // alert and keep running
	OnFailureHalt = "halt" // alert, then stop the server after FailureTimeout (ASD STIG V-222486)
)

const (
	// DefaultPort is the RFC 5425 TLS port; the operator sets 514 (UDP) or
	// 601/514 (TCP) themselves.
	DefaultPort = 6514
	// DefaultFacility is local0.
	DefaultFacility = 16
	// DefaultFailureTimeout / MinFailureTimeout bound the halt policy.
	DefaultFailureTimeout = 5 * time.Minute
	MinFailureTimeout     = time.Minute
	// QueueSize is the bounded buffer: the oldest record is dropped (and
	// counted) when a 10 001st arrives during an outage.
	QueueSize = 10000
	// MaxMessageBytes caps one syslog message (RFC 5424 §6.1: receivers
	// SHOULD accept 2048; rsyslog's default is 8 KiB). Longer messages are
	// truncated at the end of MSG on a UTF-8 boundary with " truncated=1".
	MaxMessageBytes = 8192
	// AppName is the RFC 5424 APP-NAME.
	AppName = "polarbeam-server"
)

// facilities maps RFC 5424 §6.2.1 facility names to codes.
var facilities = map[string]int{
	"kern": 0, "user": 1, "mail": 2, "daemon": 3, "auth": 4, "syslog": 5,
	"lpr": 6, "news": 7, "uucp": 8, "cron": 9, "authpriv": 10, "ftp": 11,
	"ntp": 12, "audit": 13, "alert": 14, "clock": 15,
	"local0": 16, "local1": 17, "local2": 18, "local3": 19,
	"local4": 20, "local5": 21, "local6": 22, "local7": 23,
}

// FacilityCode resolves a facility name.
func FacilityCode(name string) (int, bool) {
	c, ok := facilities[strings.ToLower(name)]
	return c, ok
}

// FacilityName is the inverse of FacilityCode ("" for an unknown code).
func FacilityName(code int) string {
	for n, c := range facilities {
		if c == code {
			return n
		}
	}
	return ""
}

// FacilityNames lists the accepted names, sorted, for error messages.
func FacilityNames() []string {
	names := make([]string, 0, len(facilities))
	for n := range facilities {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Config is one forwarding destination. It mirrors the syslog_settings row;
// the PEM fields are what the TLS transport dials with.
type Config struct {
	Enabled        bool
	Transport      string
	Host           string
	Port           int
	Framing        string
	Facility       int
	Hostname       string // RFC 5424 HOSTNAME; "" = os.Hostname()
	Content        string
	MinLevel       string // debug | info | warn | error — operational records below it are not forwarded
	OnFailure      string
	FailureTimeout time.Duration
	CAPEM          string // trust anchors for the collector; "" = system roots
	ClientCertPEM  string // optional mutual-TLS identity
	ClientKeyPEM   string
	ServerName     string // expected collector name; "" = Host
}

// Problems names every configuration problem at once (the settings-PUT
// idiom), using the API field names.
func (c Config) Problems() []string {
	var problems []string
	switch c.Transport {
	case TransportUDP, TransportTCP, TransportTLS:
	default:
		problems = append(problems, "transport must be udp, tcp, or tls")
	}
	if c.Enabled && strings.TrimSpace(c.Host) == "" {
		problems = append(problems, "host is required when forwarding is enabled")
	}
	if strings.ContainsAny(c.Host, " \t\r\n/") {
		problems = append(problems, "host must be a hostname or IP address")
	}
	if c.Port < 1 || c.Port > 65535 {
		problems = append(problems, "port must be between 1 and 65535")
	}
	switch c.Framing {
	case FramingOctetCounted:
	case FramingNonTransparent:
		if c.Transport == TransportTLS {
			problems = append(problems, "framing must be octet-counted over tls (RFC 5425)")
		}
	default:
		problems = append(problems, "framing must be octet-counted or non-transparent")
	}
	if c.Facility < 0 || c.Facility > 23 {
		problems = append(problems, "facility must be one of: "+strings.Join(FacilityNames(), ", "))
	}
	if len(c.Hostname) > 255 || strings.ContainsFunc(c.Hostname, func(r rune) bool { return r <= ' ' || r > '~' }) {
		problems = append(problems, "hostname must be printable ASCII without spaces, at most 255 characters")
	}
	switch c.Content {
	case ContentAll, ContentAudit:
	default:
		problems = append(problems, "content must be all or audit")
	}
	if _, ok := parseLevel(c.MinLevel); !ok {
		problems = append(problems, "min_level must be debug, info, warn, or error")
	}
	switch c.OnFailure {
	case OnFailureWarn, OnFailureHalt:
	default:
		problems = append(problems, "on_failure must be warn or halt")
	}
	if c.FailureTimeout < MinFailureTimeout {
		problems = append(problems, "failure_timeout must be at least 1m")
	}
	if c.CAPEM != "" {
		if err := validateCertPEM(c.CAPEM); err != nil {
			problems = append(problems, "tls_ca_pem: "+err.Error())
		}
	}
	if c.ClientCertPEM != "" {
		if c.ClientKeyPEM == "" {
			problems = append(problems, "tls_client_key_pem is required with tls_client_cert_pem")
		} else if _, err := tls.X509KeyPair([]byte(c.ClientCertPEM), []byte(c.ClientKeyPEM)); err != nil {
			problems = append(problems, "tls_client_cert_pem and tls_client_key_pem do not form a valid pair: "+err.Error())
		}
	} else if c.ClientKeyPEM != "" {
		problems = append(problems, "tls_client_key_pem without tls_client_cert_pem")
	}
	if c.ServerName != "" && strings.ContainsAny(c.ServerName, " \t\r\n/") {
		problems = append(problems, "tls_server_name must be a hostname")
	}
	return problems
}

// Warnings is advisory: choices that are valid but that an assessed
// deployment will want to know about.
func (c Config) Warnings() []string {
	var warnings []string
	if !c.Enabled {
		return warnings
	}
	switch c.Transport {
	case TransportUDP:
		warnings = append(warnings, "udp is lossy and unencrypted: delivery cannot be confirmed and records cross the network in clear text, which does not meet SC-8 at Moderate or above; use tls")
	case TransportTCP:
		warnings = append(warnings, "tcp is unencrypted: records cross the network in clear text; use tls where SC-8 applies")
	}
	if c.OnFailure == OnFailureHalt {
		warnings = append(warnings, "on_failure is halt: the control plane stops after "+c.FailureTimeout.String()+" without the collector and refuses to start while it is unreachable")
	}
	return warnings
}

func parseLevel(s string) (slog.Level, bool) {
	switch s {
	case "debug":
		return slog.LevelDebug, true
	case "info":
		return slog.LevelInfo, true
	case "warn":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	}
	return 0, false
}

func (c Config) level() slog.Level {
	l, _ := parseLevel(c.MinLevel)
	return l
}

func (c Config) addr() string { return net.JoinHostPort(c.Host, strconv.Itoa(c.Port)) }

// validateCertPEM requires at least one parseable CERTIFICATE block.
func validateCertPEM(pemText string) error {
	rest := []byte(pemText)
	certs := 0
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return fmt.Errorf("contains a non-certificate PEM block (%s)", block.Type)
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return fmt.Errorf("certificate does not parse: %w", err)
		}
		certs++
	}
	if certs == 0 {
		return fmt.Errorf("no PEM certificates found")
	}
	return nil
}

// tlsConfig builds the client TLS configuration: TLS 1.2 minimum (RFC 5425
// §4.2 requires 1.2 support; the collector may negotiate 1.3), the
// configured or system trust anchors, the collector's expected name, and
// the optional client identity. There is deliberately no way to skip
// verification.
func (c Config) tlsConfig() (*tls.Config, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: c.ServerName}
	if tc.ServerName == "" {
		tc.ServerName = c.Host
	}
	if c.CAPEM != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(c.CAPEM)) {
			return nil, fmt.Errorf("tls_ca_pem holds no usable certificate")
		}
		tc.RootCAs = pool
	}
	if c.ClientCertPEM != "" {
		pair, err := tls.X509KeyPair([]byte(c.ClientCertPEM), []byte(c.ClientKeyPEM))
		if err != nil {
			return nil, fmt.Errorf("client certificate: %w", err)
		}
		tc.Certificates = []tls.Certificate{pair}
	}
	return tc, nil
}
