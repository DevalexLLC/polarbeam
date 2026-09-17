package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"

	"github.com/devalexllc/polarbeam/internal/audit"
	"github.com/devalexllc/polarbeam/internal/server/store"
	"github.com/devalexllc/polarbeam/internal/server/syslogfwd"
)

func slogNew(h slog.Handler) *slog.Logger { return slog.New(h) }

// testKeyPair makes a self-signed certificate and its key, PEM-encoded.
func testKeyPair(t *testing.T) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "client"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
}

// fakeSyslogState backs the syslogStore fake methods.
type fakeSyslogState struct {
	syslogSettings *store.SyslogSettings
	// beforeUpdateSyslogSettings runs at the top of UpdateSyslogSettings
	// (the seam for overlapping two writes).
	beforeUpdateSyslogSettings func()
	syslogUpdates              []string // hosts, in commit order
}

func (f *fakeDB) GetSyslogSettings(_ context.Context) (*store.SyslogSettings, error) {
	if f.syslogSettings == nil {
		// Mirrors the migration-seeded defaults (disabled).
		return &store.SyslogSettings{
			Transport: "tls", Port: 6514, Framing: "octet-counted", Facility: 16,
			Content: "all", MinLevel: "info", OnFailure: "warn", FailureTimeout: 5 * time.Minute,
		}, nil
	}
	cp := *f.syslogSettings
	return &cp, nil
}

func (f *fakeDB) UpdateSyslogSettings(ctx context.Context, in store.SyslogSettings, keepClientKey bool) (*store.SyslogSettings, error) {
	if f.beforeUpdateSyslogSettings != nil {
		f.beforeUpdateSyslogSettings()
	}
	cur, _ := f.GetSyslogSettings(ctx)
	if keepClientKey {
		in.TLSClientKeyPEM = cur.TLSClientKeyPEM
	}
	in.UpdatedAt = time.Now()
	f.syslogSettings = &in
	f.syslogUpdates = append(f.syslogUpdates, in.Host)
	cp := in
	return &cp, nil
}

// fakeForwarder implements SyslogController without a socket.
type fakeForwarder struct {
	mu        sync.Mutex
	applied   []syslogfwd.Config
	revisions []time.Time
	applyErr  error
	probeErr  error
	probeRes  *syslogfwd.ProbeResult
	status    syslogfwd.Status
	// onApply runs inside Apply (the seam for observing what was emitted
	// before the sink swap).
	onApply func()
}

func (f *fakeForwarder) Apply(cfg syslogfwd.Config, rev time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.onApply != nil {
		f.onApply()
	}
	if f.applyErr != nil {
		return f.applyErr
	}
	f.applied = append(f.applied, cfg)
	f.revisions = append(f.revisions, rev)
	return nil
}

func (f *fakeForwarder) Probe(_ context.Context, cfg syslogfwd.Config) (*syslogfwd.ProbeResult, error) {
	if f.probeErr != nil {
		return nil, f.probeErr
	}
	if f.probeRes != nil {
		return f.probeRes, nil
	}
	return &syslogfwd.ProbeResult{Transport: cfg.Transport, Addr: fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)}, nil
}

func (f *fakeForwarder) Status() syslogfwd.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.status.State == "" {
		return syslogfwd.Status{State: syslogfwd.StateDisabled}
	}
	return f.status
}

func (f *fakeForwarder) hosts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.applied {
		out = append(out, c.Host)
	}
	return out
}

func syslogBody() map[string]any {
	return map[string]any{
		"enabled": true, "transport": "tcp", "host": "collector.example", "port": 601,
		"framing": "octet-counted", "facility": "local0", "hostname": "", "content": "all",
		"min_level": "info", "on_failure": "warn", "failure_timeout_ms": 300000,
		"tls_ca_pem": "", "tls_client_cert_pem": "", "tls_client_key_pem": "", "tls_server_name": "",
	}
}

func newSyslogAPI(t *testing.T) (http.Handler, *fakeDB, *fakeForwarder, *auditCapture, *http.Cookie, string) {
	t.Helper()
	f := newFakeDB()
	fwd := &fakeForwarder{}
	c := &auditCapture{}
	h := newHandler(f, testDist, &fakeProviders{}, audit.New(slogNew(c)), fwd)
	cookie, csrf := adminAndCookie(t, h, f)
	return h, f, fwd, c, cookie, csrf
}

func TestSyslogSettingsGetShapeAndStatus(t *testing.T) {
	h, f, fwd, _, cookie, _ := newSyslogAPI(t)
	f.syslogSettings = &store.SyslogSettings{
		Enabled: true, Transport: "tls", Host: "c.example", Port: 6514, Framing: "octet-counted", Facility: 10,
		Content: "audit", MinLevel: "warn", OnFailure: "halt", FailureTimeout: 2 * time.Minute,
		TLSClientCertPEM: "cert", TLSClientKeyPEM: "SECRET-KEY", UpdatedBy: "root",
	}
	fwd.status = syslogfwd.Status{State: syslogfwd.StateConnected, Buffered: 3, DroppedTotal: 7}
	w := doSettings(t, h, "GET", "/api/v1/settings/syslog", nil, cookie, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET = %d: %s", w.Code, w.Body)
	}
	body := w.Body.String()
	if strings.Contains(body, "SECRET-KEY") {
		t.Fatal("GET echoed the client private key")
	}
	var out struct {
		Facility           string `json:"facility"`
		FailureTimeoutMS   int64  `json:"failure_timeout_ms"`
		TLSClientKeyStored bool   `json:"tls_client_key_stored"`
		Status             struct {
			State        string `json:"state"`
			Buffered     int    `json:"buffered"`
			DroppedTotal uint64 `json:"dropped_total"`
		} `json:"status"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if out.Facility != "authpriv" || out.FailureTimeoutMS != 120000 || !out.TLSClientKeyStored ||
		out.Status.State != "connected" || out.Status.Buffered != 3 || out.Status.DroppedTotal != 7 {
		t.Errorf("GET shape = %+v", out)
	}
}

func TestSyslogSettingsPutValidation(t *testing.T) {
	h, _, fwd, _, cookie, csrf := newSyslogAPI(t)
	cases := []struct {
		name string
		mut  func(m map[string]any)
		want string
	}{
		{"host required when enabled", func(m map[string]any) { m["host"] = "" }, "host is required"},
		{"port range", func(m map[string]any) { m["port"] = 70000 }, "port must be"},
		{"transport enum", func(m map[string]any) { m["transport"] = "smtp" }, "transport must be"},
		{"tls forbids lf framing", func(m map[string]any) { m["transport"] = "tls"; m["framing"] = "non-transparent" }, "octet-counted over tls"},
		{"facility name", func(m map[string]any) { m["facility"] = "local9" }, "facility must be one of"},
		{"level enum", func(m map[string]any) { m["min_level"] = "loud" }, "min_level must be"},
		{"on_failure enum", func(m map[string]any) { m["on_failure"] = "explode" }, "on_failure must be"},
		{"timeout floor", func(m map[string]any) { m["failure_timeout_ms"] = 1000 }, "failure_timeout must be at least 1m"},
		{"hostname printable", func(m map[string]any) { m["hostname"] = "has space" }, "hostname must be"},
		{"ca pem parses", func(m map[string]any) { m["tls_ca_pem"] = "not pem" }, "tls_ca_pem"},
		{"key without cert", func(m map[string]any) { m["tls_client_key_pem"] = "k" }, "tls_client_key_pem without"},
		{"cert without key", func(m map[string]any) { m["tls_client_cert_pem"] = "c" }, "tls_client_key_pem is required"},
		{"mismatched pair", func(m map[string]any) { m["tls_client_cert_pem"] = "c"; m["tls_client_key_pem"] = "k" }, "valid pair"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := syslogBody()
			tc.mut(body)
			w := doSettings(t, h, "PUT", "/api/v1/settings/syslog", body, cookie, csrf)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), tc.want) {
				t.Errorf("= %d %s, want 400 mentioning %q", w.Code, w.Body, tc.want)
			}
		})
	}
	if len(fwd.applied) != 0 {
		t.Error("a rejected write reached Apply")
	}
	// Unknown fields are refused (decodeStrict).
	body := syslogBody()
	body["insecure_skip_verify"] = true
	if w := doSettings(t, h, "PUT", "/api/v1/settings/syslog", body, cookie, csrf); w.Code != http.StatusBadRequest {
		t.Errorf("unknown field = %d", w.Code)
	}
}

func TestSyslogSettingsPutStoresAppliesAndWarns(t *testing.T) {
	h, f, fwd, c, cookie, csrf := newSyslogAPI(t)
	body := syslogBody()
	body["transport"] = "udp"
	body["port"] = 514
	body["on_failure"] = "halt"
	w := doSettings(t, h, "PUT", "/api/v1/settings/syslog", body, cookie, csrf)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", w.Code, w.Body)
	}
	if f.syslogSettings == nil || f.syslogSettings.Host != "collector.example" || f.syslogSettings.UpdatedBy != "root" {
		t.Errorf("stored = %+v", f.syslogSettings)
	}
	if hosts := fwd.hosts(); len(hosts) != 1 || hosts[0] != "collector.example" || !fwd.revisions[0].Equal(f.syslogSettings.UpdatedAt) {
		t.Errorf("applied = %v revisions = %v", hosts, fwd.revisions)
	}
	if !strings.Contains(w.Body.String(), "udp is lossy") || !strings.Contains(w.Body.String(), "on_failure is halt") {
		t.Errorf("warnings missing: %s", w.Body)
	}
	// One record, the handler's own, carrying the change and the actor —
	// and no api.write for the route.
	recs := c.records(audit.EventSyslogSettingsUpdate)
	if len(recs) != 1 || attr(recs[0], "user") != "root" || attr(recs[0], "host") != "collector.example" ||
		attr(recs[0], "previous_enabled") != "false" || attr(recs[0], "enabled") != "true" || attr(recs[0], "route") != "PUT /api/v1/settings/syslog" {
		t.Errorf("update records = %d: %s", len(recs), recordString(recs[0]))
	}
	for _, r := range c.records(audit.EventAPIWrite) {
		if attr(r, "route") == "PUT /api/v1/settings/syslog" {
			t.Error("syslog PUT also emitted api.write")
		}
	}
}

// TestSyslogSettingsRecordReachesTheRightSink: when a destination was
// enabled, the change record is emitted BEFORE Apply retires it (so the
// previous collector gets it); on first enable it is emitted after Apply
// (so the new collector gets it).
func TestSyslogSettingsRecordReachesTheRightSink(t *testing.T) {
	h, f, fwd, c, cookie, csrf := newSyslogAPI(t)
	var seenAtApply []int
	fwd.onApply = func() { seenAtApply = append(seenAtApply, len(c.records(audit.EventSyslogSettingsUpdate))) }

	// First enable: nothing to deliver to yet → record after Apply.
	if w := doSettings(t, h, "PUT", "/api/v1/settings/syslog", syslogBody(), cookie, csrf); w.Code != http.StatusOK {
		t.Fatalf("enable = %d: %s", w.Code, w.Body)
	}
	// Move: the record must be queued before the old sink is retired.
	body := syslogBody()
	body["host"] = "other.example"
	if w := doSettings(t, h, "PUT", "/api/v1/settings/syslog", body, cookie, csrf); w.Code != http.StatusOK {
		t.Fatalf("move = %d: %s", w.Code, w.Body)
	}
	// Disable: likewise before Apply.
	body["enabled"] = false
	if w := doSettings(t, h, "PUT", "/api/v1/settings/syslog", body, cookie, csrf); w.Code != http.StatusOK {
		t.Fatalf("disable = %d: %s", w.Code, w.Body)
	}
	if want := []int{0, 2, 3}; fmt.Sprint(seenAtApply) != fmt.Sprint(want) {
		t.Errorf("update records visible at Apply = %v, want %v (after on first enable, before on move and disable)", seenAtApply, want)
	}
	recs := c.records(audit.EventSyslogSettingsUpdate)
	if attr(recs[1], "previous_host") != "collector.example" || attr(recs[1], "host") != "other.example" ||
		attr(recs[2], "enabled") != "false" || attr(recs[2], "previous_enabled") != "true" {
		t.Errorf("move/disable records: %s / %s", recordString(recs[1]), recordString(recs[2]))
	}
	if !f.syslogSettings.Enabled == false && fwd.hosts()[2] != "" {
		t.Error("disable did not reach Apply")
	}
}

// TestSyslogSettingsClientKeyWriteOnly: the key is kept when a certificate
// is resubmitted without it, replaced when given, cleared with the
// certificate, and always validated as the effective pair.
func TestSyslogSettingsClientKeyWriteOnly(t *testing.T) {
	h, f, _, _, cookie, csrf := newSyslogAPI(t)
	certA, keyA := testKeyPair(t)
	certB, keyB := testKeyPair(t)
	body := syslogBody()
	body["transport"], body["port"] = "tls", 6514
	body["tls_client_cert_pem"], body["tls_client_key_pem"] = certA, keyA
	if w := doSettings(t, h, "PUT", "/api/v1/settings/syslog", body, cookie, csrf); w.Code != http.StatusOK {
		t.Fatalf("store pair = %d: %s", w.Code, w.Body)
	}
	if f.syslogSettings.TLSClientKeyPEM != keyA {
		t.Fatal("key not stored")
	}
	// Same cert, blank key: kept.
	body["tls_client_key_pem"] = ""
	body["host"] = "moved.example"
	if w := doSettings(t, h, "PUT", "/api/v1/settings/syslog", body, cookie, csrf); w.Code != http.StatusOK {
		t.Fatalf("keep key = %d: %s", w.Code, w.Body)
	}
	if f.syslogSettings.TLSClientKeyPEM != keyA || f.syslogSettings.Host != "moved.example" {
		t.Error("stored key not kept on a blank submission")
	}
	// New cert with the retained key: refused as a mismatched pair before
	// anything is persisted.
	body["tls_client_cert_pem"] = certB
	if w := doSettings(t, h, "PUT", "/api/v1/settings/syslog", body, cookie, csrf); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "valid pair") {
		t.Errorf("cert B + retained key A = %d: %s", w.Code, w.Body)
	}
	if f.syslogSettings.TLSClientCertPEM != certA {
		t.Error("mismatched pair was persisted")
	}
	// New cert with its key: replaced.
	body["tls_client_key_pem"] = keyB
	if w := doSettings(t, h, "PUT", "/api/v1/settings/syslog", body, cookie, csrf); w.Code != http.StatusOK {
		t.Fatalf("replace pair = %d: %s", w.Code, w.Body)
	}
	if f.syslogSettings.TLSClientKeyPEM != keyB {
		t.Error("key not replaced")
	}
	// Clearing the certificate clears the key.
	body["tls_client_cert_pem"], body["tls_client_key_pem"] = "", ""
	if w := doSettings(t, h, "PUT", "/api/v1/settings/syslog", body, cookie, csrf); w.Code != http.StatusOK {
		t.Fatalf("clear = %d: %s", w.Code, w.Body)
	}
	if f.syslogSettings.TLSClientKeyPEM != "" || f.syslogSettings.TLSClientCertPEM != "" {
		t.Error("clearing the certificate left a key behind")
	}
}

// TestSyslogSettingsWritesSerialize: two overlapping writes commit and
// apply in the same order (the handler holds one mutex across
// read-validate-store-apply).
func TestSyslogSettingsWritesSerialize(t *testing.T) {
	h, f, fwd, _, cookie, csrf := newSyslogAPI(t)
	f.beforeUpdateSyslogSettings = func() { time.Sleep(30 * time.Millisecond) }
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := syslogBody()
			body["host"] = fmt.Sprintf("c%d.example", i)
			if w := doSettings(t, h, "PUT", "/api/v1/settings/syslog", body, cookie, csrf); w.Code != http.StatusOK {
				t.Errorf("PUT %d = %d", i, w.Code)
			}
		}(i)
	}
	wg.Wait()
	if fmt.Sprint(f.syslogUpdates) != fmt.Sprint(fwd.hosts()) {
		t.Errorf("commit order %v != apply order %v", f.syslogUpdates, fwd.hosts())
	}
	if f.syslogSettings.Host != f.syslogUpdates[len(f.syslogUpdates)-1] {
		t.Error("GET would not match the last applied")
	}
}

func TestSyslogSettingsTest(t *testing.T) {
	h, _, fwd, _, cookie, csrf := newSyslogAPI(t)
	body := syslogBody()
	body["enabled"] = false // testing does not require enabling first
	w := doSettings(t, h, "POST", "/api/v1/settings/syslog/test", body, cookie, csrf)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"addr":"collector.example:601"`) {
		t.Errorf("test = %d: %s", w.Code, w.Body)
	}
	body["host"] = ""
	if w := doSettings(t, h, "POST", "/api/v1/settings/syslog/test", body, cookie, csrf); w.Code != http.StatusBadRequest {
		t.Errorf("test without host = %d", w.Code)
	}
	fwd.probeErr = errors.New("x509: certificate signed by unknown authority")
	body["host"] = "collector.example"
	w = doSettings(t, h, "POST", "/api/v1/settings/syslog/test", body, cookie, csrf)
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "unknown authority") {
		t.Errorf("failed test = %d: %s", w.Code, w.Body)
	}
	if len(fwd.applied) != 0 {
		t.Error("test touched the live sink")
	}
}

func TestSyslogSettingsRequireAdmin(t *testing.T) {
	h, f, _, _, _, _ := newSyslogAPI(t)
	viewer, viewerCSRF := loginRole(t, h, f, "vic", "viewer")
	for _, rt := range []struct{ method, path string }{
		{"GET", "/api/v1/settings/syslog"}, {"PUT", "/api/v1/settings/syslog"}, {"POST", "/api/v1/settings/syslog/test"},
	} {
		if w := doSettings(t, h, rt.method, rt.path, syslogBody(), viewer, viewerCSRF); w.Code != http.StatusForbidden {
			t.Errorf("%s %s as viewer = %d", rt.method, rt.path, w.Code)
		}
	}
	_ = httptest.NewRecorder
}
