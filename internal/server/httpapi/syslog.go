// Settings -> Log forwarding: the syslog destination the server forwards
// audit (and optionally all) records to. DB-backed like the OIDC settings
// and applied to the running forwarder without a restart. The client
// private key is write-only, like the OIDC client secret.

package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/devalexllc/polarbeam/internal/audit"
	"github.com/devalexllc/polarbeam/internal/server/store"
	"github.com/devalexllc/polarbeam/internal/server/syslogfwd"
)

// SyslogController is the running forwarder as the handlers see it; tests
// pass a fake so no handler test opens a socket.
type SyslogController interface {
	Apply(cfg syslogfwd.Config, revision time.Time) error
	Probe(ctx context.Context, cfg syslogfwd.Config) (*syslogfwd.ProbeResult, error)
	Status() syslogfwd.Status
}

// probeTimeout bounds the Test button's dial + handshake + write.
const probeTimeout = 15 * time.Second

type syslogSettingsJSON struct {
	Enabled          bool   `json:"enabled"`
	Transport        string `json:"transport"`
	Host             string `json:"host"`
	Port             int    `json:"port"`
	Framing          string `json:"framing"`
	Facility         string `json:"facility"`
	Hostname         string `json:"hostname"`
	Content          string `json:"content"`
	MinLevel         string `json:"min_level"`
	OnFailure        string `json:"on_failure"`
	FailureTimeoutMS int64  `json:"failure_timeout_ms"`
	TLSCAPEM         string `json:"tls_ca_pem"`
	TLSClientCertPEM string `json:"tls_client_cert_pem"`
	// The key is write-only: GET reports only whether one is stored.
	TLSClientKeyStored bool              `json:"tls_client_key_stored"`
	TLSServerName      string            `json:"tls_server_name"`
	UpdatedAt          time.Time         `json:"updated_at"`
	UpdatedBy          string            `json:"updated_by"`
	Status             *syslogfwd.Status `json:"status,omitempty"`
	// Warnings is advisory and set only on a write response.
	Warnings []string `json:"warnings,omitempty"`
}

type syslogSettingsRequest struct {
	Enabled          bool   `json:"enabled"`
	Transport        string `json:"transport"`
	Host             string `json:"host"`
	Port             int    `json:"port"`
	Framing          string `json:"framing"`
	Facility         string `json:"facility"`
	Hostname         string `json:"hostname"`
	Content          string `json:"content"`
	MinLevel         string `json:"min_level"`
	OnFailure        string `json:"on_failure"`
	FailureTimeoutMS int64  `json:"failure_timeout_ms"`
	TLSCAPEM         string `json:"tls_ca_pem"`
	TLSClientCertPEM string `json:"tls_client_cert_pem"`
	// Empty means "keep the stored key" while a client certificate is
	// submitted; an empty certificate clears both.
	TLSClientKeyPEM string `json:"tls_client_key_pem"`
	TLSServerName   string `json:"tls_server_name"`
}

func toSyslogSettingsJSON(s *store.SyslogSettings) syslogSettingsJSON {
	return syslogSettingsJSON{
		Enabled: s.Enabled, Transport: s.Transport, Host: s.Host, Port: s.Port,
		Framing: s.Framing, Facility: syslogfwd.FacilityName(s.Facility), Hostname: s.Hostname,
		Content: s.Content, MinLevel: s.MinLevel, OnFailure: s.OnFailure,
		FailureTimeoutMS: s.FailureTimeout.Milliseconds(),
		TLSCAPEM:         s.TLSCAPEM, TLSClientCertPEM: s.TLSClientCertPEM,
		TLSClientKeyStored: s.TLSClientKeyPEM != "", TLSServerName: s.TLSServerName,
		UpdatedAt: s.UpdatedAt, UpdatedBy: s.UpdatedBy,
	}
}

// resolveSyslogSettings turns a request into the row to store and the
// effective configuration, naming every problem. The client key is
// resolved against the STORED row (submitted, else kept while a
// certificate is submitted) and the effective pair is validated before
// anything is persisted — a mismatched pair must be refused here, not
// discovered by the writer (which under halt would stop the server).
// keepKey tells the store to preserve its key column.
func resolveSyslogSettings(in syslogSettingsRequest, current *store.SyslogSettings) (row store.SyslogSettings, keepKey bool, cfg syslogfwd.Config, problems, warnings []string) {
	facility, ok := syslogfwd.FacilityCode(in.Facility)
	if !ok {
		problems = append(problems, "facility must be one of: "+strings.Join(syslogfwd.FacilityNames(), ", "))
	}
	key := in.TLSClientKeyPEM
	switch {
	case in.TLSClientCertPEM == "" && key == "":
		// Clearing the certificate clears the stored key too; a key
		// submitted without a certificate is left for Problems to refuse.
	case in.TLSClientCertPEM != "" && key == "" && current.TLSClientKeyPEM != "":
		key, keepKey = current.TLSClientKeyPEM, true
	}
	row = store.SyslogSettings{
		Enabled: in.Enabled, Transport: in.Transport, Host: strings.TrimSpace(in.Host), Port: in.Port,
		Framing: in.Framing, Facility: facility, Hostname: strings.TrimSpace(in.Hostname),
		Content: in.Content, MinLevel: in.MinLevel, OnFailure: in.OnFailure,
		FailureTimeout: time.Duration(in.FailureTimeoutMS) * time.Millisecond,
		TLSCAPEM:       in.TLSCAPEM, TLSClientCertPEM: in.TLSClientCertPEM, TLSClientKeyPEM: key,
		TLSServerName: strings.TrimSpace(in.TLSServerName),
	}
	cfg = syslogfwd.FromSettings(&row)
	problems = append(problems, cfg.Problems()...)
	return row, keepKey, cfg, problems, cfg.Warnings()
}

func (a *api) handleSyslogSettingsGet(w http.ResponseWriter, r *http.Request) {
	s, err := a.db.GetSyslogSettings(r.Context())
	if err != nil {
		internalError(w, "get syslog settings", err)
		return
	}
	out := toSyslogSettingsJSON(s)
	st := a.forwarder.Status()
	out.Status = &st
	writeJSON(w, http.StatusOK, out)
}

// handleSyslogSettingsPut validates, stores, records, and applies — all
// under one mutex, so two concurrent writes cannot interleave a key
// resolved against one row with a write that retains another, and they
// commit and apply in the same order (the forwarder also refuses a stale
// revision). The settings-change record is the handler's own
// (settings.syslog.update, middleware suppressed): when a destination was
// enabled it is queued BEFORE Apply retires that sink, so the previous
// collector receives the record of its own retirement; when forwarding
// was off it is emitted after Apply, so the newly enabled collector
// receives it as its first record after audit.forward.start.
func (a *api) handleSyslogSettingsPut(w http.ResponseWriter, r *http.Request) {
	var in syslogSettingsRequest
	if !decodeStrict(w, r, &in) {
		return
	}
	a.syslogMu.Lock()
	defer a.syslogMu.Unlock()
	current, err := a.db.GetSyslogSettings(r.Context())
	if err != nil {
		internalError(w, "get syslog settings", err)
		return
	}
	row, keepKey, _, problems, warnings := resolveSyslogSettings(in, current)
	if len(problems) > 0 {
		writeError(w, http.StatusBadRequest, strings.Join(problems, "; "))
		return
	}
	row.UpdatedBy = sessionFrom(r.Context()).Username
	out, err := a.db.UpdateSyslogSettings(r.Context(), row, keepKey)
	if err != nil {
		internalError(w, "update syslog settings", err)
		return
	}
	audit.Suppress(r.Context())
	record := func() {
		a.audit.Emit(r.Context(), audit.Event{
			ID: audit.EventSyslogSettingsUpdate, Msg: "syslog forwarding settings changed", Outcome: audit.Success,
			Actor: actorFrom(sessionFrom(r.Context())), Remote: clientIP(r),
			Attrs: append(routeAttrs(r),
				slog.Bool("enabled", out.Enabled), slog.String("transport", out.Transport),
				slog.String("host", out.Host), slog.Int("port", out.Port),
				slog.String("content", out.Content), slog.String("on_failure", out.OnFailure),
				slog.Bool("previous_enabled", current.Enabled), slog.String("previous_host", current.Host),
				slog.Int("previous_port", current.Port)),
		})
	}
	if current.Enabled {
		record()
	}
	if err := a.forwarder.Apply(syslogfwd.FromSettings(out), out.UpdatedAt); err != nil {
		// The row validated above; a refusal here is a bug, and loud.
		internalError(w, "apply syslog settings", err)
		return
	}
	if !current.Enabled {
		record()
	}
	resp := toSyslogSettingsJSON(out)
	resp.Warnings = warnings
	st := a.forwarder.Status()
	resp.Status = &st
	writeJSON(w, http.StatusOK, resp)
}

// handleSyslogSettingsTest dials the SUBMITTED configuration (stored key
// when the form leaves it blank), completes the handshake, and sends one
// test record, without saving anything: the loud surface for reachability
// and PKI problems. 502 because the collector is what failed.
func (a *api) handleSyslogSettingsTest(w http.ResponseWriter, r *http.Request) {
	var in syslogSettingsRequest
	if !decodeStrict(w, r, &in) {
		return
	}
	current, err := a.db.GetSyslogSettings(r.Context())
	if err != nil {
		internalError(w, "get syslog settings", err)
		return
	}
	in.Enabled = true // a test needs a destination whether or not it is enabled yet
	_, _, cfg, problems, _ := resolveSyslogSettings(in, current)
	if len(problems) > 0 {
		writeError(w, http.StatusBadRequest, strings.Join(problems, "; "))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()
	res, err := a.forwarder.Probe(ctx, cfg)
	if err != nil {
		writeError(w, http.StatusBadGateway, "collector test failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}
