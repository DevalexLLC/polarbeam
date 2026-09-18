package syslogfwd

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/devalexllc/polarbeam/internal/audit"
)

func init() {
	// Fast timings for the tests; the production values are documented
	// on the variables.
	dialTimeout = 2 * time.Second
	writeTimeout = 2 * time.Second
	backoffMin = 20 * time.Millisecond
	backoffMax = 100 * time.Millisecond
	drainTimeout = 2 * time.Second
	stableAfter = 300 * time.Millisecond
}

// receiver is a loopback collector: it accepts connections, parses
// octet-counted (or LF-delimited) frames, and exposes them on msgs.
type receiver struct {
	t      *testing.T
	ln     net.Listener
	addr   string
	tlsCfg *tls.Config
	lf     bool
	msgs   chan string
	mu     sync.Mutex
	conns  []net.Conn
	closed bool
}

func newReceiver(t *testing.T, tlsCfg *tls.Config, lf bool) *receiver {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &receiver{t: t, ln: ln, addr: ln.Addr().String(), tlsCfg: tlsCfg, lf: lf, msgs: make(chan string, 20000)}
	go r.serve(ln)
	t.Cleanup(r.stop)
	return r
}

func (r *receiver) hostPort() (string, int) {
	h, p, _ := net.SplitHostPort(r.addr)
	n, _ := strconv.Atoi(p)
	return h, n
}

func (r *receiver) serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		if r.tlsCfg != nil {
			c = tls.Server(c, r.tlsCfg)
		}
		r.mu.Lock()
		r.conns = append(r.conns, c)
		r.mu.Unlock()
		go r.read(c)
	}
}

func (r *receiver) read(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	for {
		var msg string
		if r.lf {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			msg = strings.TrimSuffix(line, "\n")
		} else {
			lenStr, err := br.ReadString(' ')
			if err != nil {
				return
			}
			n, err := strconv.Atoi(strings.TrimSpace(lenStr))
			if err != nil {
				r.t.Errorf("bad octet count %q", lenStr)
				return
			}
			buf := make([]byte, n)
			if _, err := io.ReadFull(br, buf); err != nil {
				return
			}
			msg = string(buf)
		}
		r.msgs <- msg
	}
}

// dropConns closes accepted connections (the collector "going away"
// without closing the listener) — closing only the listener would leave
// established connections open.
func (r *receiver) dropConns() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.conns {
		c.Close()
	}
	r.conns = nil
}

// stop closes the listener and every connection.
func (r *receiver) stop() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	r.mu.Unlock()
	r.ln.Close()
	r.dropConns()
}

// restart listens again on the same address.
func (r *receiver) restart() {
	r.t.Helper()
	r.stop()
	var ln net.Listener
	var err error
	for i := 0; i < 50; i++ {
		ln, err = net.Listen("tcp", r.addr)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		r.t.Fatalf("re-listen on %s: %v", r.addr, err)
	}
	r.mu.Lock()
	r.ln, r.closed = ln, false
	r.mu.Unlock()
	go r.serve(ln)
}

// waitConn waits until the collector has accepted at least one
// connection: the forwarder's "connected" is the kernel handshake, which
// precedes Accept.
func (r *receiver) waitConn(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		n := len(r.conns)
		r.mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("collector accepted no connection")
}

// next waits for a message.
func (r *receiver) next(t *testing.T) string {
	t.Helper()
	select {
	case m := <-r.msgs:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("no message within 5s")
		return ""
	}
}

func (r *receiver) none(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case m := <-r.msgs:
		t.Fatalf("unexpected message: %s", m)
	case <-time.After(d):
	}
}

var seqRe = regexp.MustCompile(`\[meta sequenceId="(\d+)"\]`)

func seqOf(t *testing.T, msg string) int {
	t.Helper()
	m := seqRe.FindStringSubmatch(msg)
	if m == nil {
		t.Fatalf("no sequenceId in %q", msg)
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func tcpConfig(r *receiver) Config {
	c := Defaults()
	c.Enabled, c.Transport, c.Framing = true, TransportTCP, FramingOctetCounted
	c.Host, c.Port = r.hostPort()
	return c
}

// installDefault routes slog.Default through the forwarder for the test,
// so the forwarder's own audit records (start/disconnected/recovered)
// reach the queue as they do in production behind Tee.
func installDefault(t *testing.T, f *Forwarder) {
	t.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(Tee(slog.DiscardHandler, f)))
	t.Cleanup(func() { slog.SetDefault(prev) })
}

func newTestForwarder(t *testing.T) *Forwarder {
	t.Helper()
	f := New(Options{Version: "v1.2.3", Hostname: "cp.example"})
	t.Cleanup(func() { f.Close(context.Background()) })
	return f
}

// --- format ---

func TestMessageGolden(t *testing.T) {
	fm := formatter{hostname: "cp.example", procID: "4242", version: "v1.2.3"}
	at := time.Date(2026, 9, 17, 14, 3, 22, 418233000, time.UTC)
	body := []byte(`msg="agent enrolled" event=agent.enroll outcome=success user=8b2f remote=203.0.113.9`)
	params := []sdParam{{"event", "agent.enroll"}, {"outcome", "success"}, {"user", "8b2f"}, {"remote", "203.0.113.9"}}
	got := string(fm.message(16, slog.LevelInfo, at, "agent.enroll", 7, "10.0.0.5", params, body))
	want := "<134>1 2026-09-17T14:03:22.418233Z cp.example polarbeam-server 4242 agent.enroll " +
		`[timeQuality tzKnown="1"][origin software="polarbeam-server" swVersion="v1.2.3" ip="10.0.0.5"][meta sequenceId="7"]` +
		`[polarbeam@66894 event="agent.enroll" outcome="success" user="8b2f" remote="203.0.113.9"] ` +
		"\xEF\xBB\xBF" + string(body)
	if got != want {
		t.Errorf("message =\n%q\nwant\n%q", got, want)
	}
	// Operational record: NIL msgid, Warn severity, no origin ip before the
	// first connection, an escaped SD value, no enterprise element.
	fm.version = `v"1]\`
	got = string(fm.message(4, slog.LevelWarn, at, "", 1, "", nil, []byte("x")))
	if !strings.HasPrefix(got, "<36>1 2026-09-17T14:03:22.418233Z cp.example polarbeam-server 4242 - [timeQuality") ||
		!strings.Contains(got, `swVersion="v\"1\]\\"][meta`) || strings.Contains(got, ` ip=`) ||
		!strings.Contains(got, `"] `+"\xEF\xBB\xBF"+`x`) || strings.Contains(got, EnterpriseSDID) {
		t.Errorf("operational message = %q", got)
	}
	// Enterprise PARAM-VALUEs are escaped and clipped on a rune boundary.
	long := strings.Repeat("é", maxSDParamValue) // 2 bytes each: clipped mid-rune unless the cut backs up
	got = string(fm.message(16, slog.LevelInfo, at, "e", 1, "", []sdParam{{"user", `a"b]c\`}, {"remote", long}}, []byte("x")))
	wantUser := `[polarbeam@66894 user="a\"b\]c\\" remote="` + strings.Repeat("é", maxSDParamValue/2) + `"] `
	if !strings.Contains(got, wantUser) || !utf8.ValidString(got) {
		t.Errorf("escaped/clipped element missing in %q", got)
	}
}

// sdDecode is what a standards-following receiver plus our documented
// percent step yields: RFC 5424 §6.3.3 unescaping (a backslash before
// '"', '\\', or ']' is dropped; before anything else it is kept), then
// percent-decoding.
func sdDecode(t *testing.T, v string) string {
	t.Helper()
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] == '\\' && i+1 < len(v) && strings.IndexByte(`"\]`, v[i+1]) >= 0 {
			i++
		}
		b.WriteByte(v[i])
	}
	out, err := url.PathUnescape(b.String())
	if err != nil {
		t.Fatalf("percent-decode %q: %v", b.String(), err)
	}
	return out
}

// A claimed username from a failed login reaches the enterprise element
// unfiltered. Control characters in a PARAM-VALUE must be encoded: a raw
// LF would split a non-transparent-framed record on the collector, and
// CR/NUL confuse header parsers. The encoding must also stay reversible
// after the receiver's RFC unescaping, so distinct claimed identities
// stay distinct in the indexed field.
func TestSDValueControlChars(t *testing.T) {
	fm := formatter{hostname: "h", procID: "1", version: "v"}
	at := time.Date(2026, 9, 17, 14, 3, 22, 0, time.UTC)
	values := []string{
		"eve\ninjected\r\x00\ttab\x7f",
		`a\nb`, "a\nb", `a\\nb`, "a%0Ab", "100%", `x"y]z\`, "tzürich é", "",
	}
	seen := map[string]string{}
	for _, v := range values {
		got := fm.message(16, slog.LevelWarn, at, "auth.login", 1, "", []sdParam{{"user", v}}, []byte("x"))
		sd := string(got[:bytes.Index(got, bom)])
		if strings.ContainsAny(sd, "\n\r\x00\t\x7f") || !utf8.Valid(got) {
			t.Errorf("%q: control character or invalid UTF-8 in header/SD: %q", v, sd)
		}
		wire := strings.TrimSuffix(strings.SplitN(sd, `[polarbeam@66894 user="`, 2)[1], `"] `)
		if dec := sdDecode(t, wire); dec != v {
			t.Errorf("%q: wire %q decodes to %q", v, wire, dec)
		}
		if prev, dup := seen[wire]; dup {
			t.Errorf("%q and %q share the wire spelling %q", prev, v, wire)
		}
		seen[wire] = v
	}
	// Spellings are the documented ones.
	got := string(fm.message(16, slog.LevelWarn, at, "e", 1, "", []sdParam{{"user", "a\nb%"}}, []byte("x")))
	if !strings.Contains(got, `[polarbeam@66894 user="a%0Ab%25"] `) {
		t.Errorf("spelling: %q", got)
	}
}

func TestAuditParams(t *testing.T) {
	rec := func(attrs ...slog.Attr) slog.Record {
		r := slog.NewRecord(time.Now(), slog.LevelInfo, "m", 0)
		r.AddAttrs(attrs...)
		return r
	}
	// Operational record (event key absent, or present but not first): no element.
	if p := auditParams(rec(slog.String("site", "nyc"))); p != nil {
		t.Errorf("operational params = %v, want nil", p)
	}
	if p := auditParams(rec(slog.String("site", "nyc"), slog.String("event", "x"))); p != nil {
		t.Errorf("non-first event params = %v, want nil", p)
	}
	// Audit record: the fixed fields in Emit order, absent ones skipped,
	// event-specific attributes ignored, and a repeated key keeps its first
	// value (an event-specific attribute cannot shadow a fixed one).
	got := auditParams(rec(
		slog.String("event", "auth.login"), slog.String("outcome", "failure"),
		slog.String("remote", "198.51.100.7:4433"), slog.String("user_source", "local"),
		slog.String("reason", "bad password"), slog.Int("attempts", 3),
		slog.String("remote", "spoofed"), slog.String("user", "alice"),
	))
	want := []sdParam{{"event", "auth.login"}, {"outcome", "failure"}, {"user", "alice"}, {"user_source", "local"}, {"remote", "198.51.100.7:4433"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("auditParams = %v, want %v", got, want)
	}
	if got := eventParams("syslog.forward.stop", "success"); !reflect.DeepEqual(got, []sdParam{{"event", "syslog.forward.stop"}, {"outcome", "success"}}) {
		t.Errorf("eventParams = %v", got)
	}
}

func TestSeverityAndFacility(t *testing.T) {
	for l, want := range map[slog.Level]int{slog.LevelDebug: 7, slog.LevelInfo: 6, slog.LevelWarn: 4, slog.LevelError: 3} {
		if got := severity(l); got != want {
			t.Errorf("severity(%v) = %d, want %d", l, got, want)
		}
	}
	if c, ok := FacilityCode("authpriv"); !ok || c != 10 || FacilityName(16) != "local0" {
		t.Error("facility mapping")
	}
}

func TestTruncateRuneBoundary(t *testing.T) {
	head := "<134>1 2026-09-17T14:03:22.418233Z h a p - [] \xEF\xBB\xBF"
	msg := []byte(head + strings.Repeat("é", 6000))
	out := truncate(msg)
	if len(out) > MaxMessageBytes || !utf8.Valid(out) || !strings.HasSuffix(string(out), truncatedMark) {
		t.Errorf("truncated len=%d valid=%v suffix=%v", len(out), utf8.Valid(out), strings.HasSuffix(string(out), truncatedMark))
	}
	if short := []byte("short"); string(truncate(short)) != "short" {
		t.Error("short message altered")
	}
}

func TestFrames(t *testing.T) {
	msg := []byte("hello")
	if got := string(frame(msg, TransportTCP, FramingOctetCounted)); got != "5 hello" {
		t.Errorf("octet-counted = %q", got)
	}
	if got := string(frame(msg, TransportTCP, FramingNonTransparent)); got != "hello\n" {
		t.Errorf("non-transparent = %q", got)
	}
	if got := string(frame(msg, TransportUDP, FramingNonTransparent)); got != "hello" {
		t.Errorf("udp = %q", got)
	}
}

func TestConfigProblems(t *testing.T) {
	c := Defaults()
	c.Enabled = true
	c.Transport, c.Framing, c.Port, c.Facility, c.Content, c.MinLevel, c.OnFailure = "smtp", "lf", 0, 99, "some", "loud", "panic"
	c.FailureTimeout, c.Hostname, c.ClientKeyPEM = time.Second, "has space", "key"
	problems := strings.Join(c.Problems(), "\n")
	for _, want := range []string{"transport", "host is required", "port", "framing", "facility", "hostname", "content", "min_level", "on_failure", "failure_timeout", "tls_client_key_pem without"} {
		if !strings.Contains(problems, want) {
			t.Errorf("problems lack %q:\n%s", want, problems)
		}
	}
	if c := Defaults(); len(c.Problems()) != 0 {
		t.Errorf("defaults have problems: %v", c.Problems())
	}
	c = Defaults()
	c.Enabled, c.Host, c.Framing = true, "x", FramingNonTransparent
	if p := c.Problems(); len(p) != 1 || !strings.Contains(p[0], "octet-counted over tls") {
		t.Errorf("tls + non-transparent problems = %v", p)
	}
	c.Transport = TransportUDP
	if w := c.Warnings(); len(w) != 1 || !strings.Contains(w[0], "udp") {
		t.Errorf("udp warnings = %v", w)
	}
}

// --- transport ---

func TestRoundTripTCPAndWithAttrs(t *testing.T) {
	r := newReceiver(t, nil, false)
	f := newTestForwarder(t)
	installDefault(t, f)
	if err := f.Apply(tcpConfig(r), time.Now()); err != nil {
		t.Fatal(err)
	}
	start := r.next(t)
	if !strings.Contains(start, "event="+audit.EventForwardStart) || seqOf(t, start) != 1 {
		t.Errorf("first message = %s", start)
	}
	l := slog.New(f).With("site", "nyc")
	l.Info("hello", "k", "v")
	m := r.next(t)
	if !strings.Contains(m, ` cp.example polarbeam-server `) || !strings.Contains(m, ` - [timeQuality`) || !strings.Contains(m, `msg=hello site=nyc k=v`) || !strings.Contains(m, `ip="127.0.0.1"`) || seqOf(t, m) != 2 {
		t.Errorf("message = %s", m)
	}
	// The configured Hostname overrides the process default.
	named := tcpConfig(r)
	named.Hostname = "cp.override"
	if err := f.Apply(named, time.Now()); err != nil {
		t.Fatal(err)
	}
	if m := r.next(t); !strings.Contains(m, ` cp.override polarbeam-server `) {
		t.Errorf("hostname override missing: %s", m)
	}
	// The receiver can read the frame before the writer's pop lands; wait
	// for the queue to drain rather than asserting on the instant.
	waitBuffered(t, f, 0)
	if st := f.Status(); st.State != StateConnected {
		t.Errorf("status = %+v", st)
	}
	// Audit records pass regardless of MinLevel; operational ones below it
	// and, with content=audit, all of them, do not.
	cfg := tcpConfig(r)
	cfg.MinLevel = "warn"
	if err := f.Apply(cfg, time.Now()); err != nil {
		t.Fatal(err)
	}
	r.next(t) // start record on the new sink
	l.Info("dropped operational")
	audit.New(slog.New(f)).Emit(context.Background(), audit.Event{ID: audit.EventLogin, Msg: "login", Outcome: audit.Success})
	if m := r.next(t); !strings.Contains(m, "event=auth.login") || !strings.HasPrefix(m, "<134>1 ") || !strings.Contains(m, " auth.login [") ||
		!strings.Contains(m, `[`+EnterpriseSDID+` event="auth.login" outcome="success"] `) {
		t.Errorf("audit message = %s", m)
	}
	cfg.Content = ContentAudit
	if err := f.Apply(cfg, time.Now()); err != nil {
		t.Fatal(err)
	}
	r.next(t)
	l.Error("operational error not forwarded")
	r.none(t, 200*time.Millisecond)
}

func TestNonTransparentAndUDP(t *testing.T) {
	r := newReceiver(t, nil, true)
	f := newTestForwarder(t)
	cfg := tcpConfig(r)
	cfg.Framing = FramingNonTransparent
	if err := f.Apply(cfg, time.Now()); err != nil {
		t.Fatal(err)
	}
	slog.New(f).Info("line one")
	if m := r.next(t); !strings.HasSuffix(m, `msg="line one"`) {
		t.Errorf("lf message = %s", m)
	}

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	h, p, _ := net.SplitHostPort(pc.LocalAddr().String())
	cfg = Defaults()
	cfg.Enabled, cfg.Transport, cfg.Host = true, TransportUDP, h
	cfg.Port, _ = strconv.Atoi(p)
	if err := f.Apply(cfg, time.Now()); err != nil {
		t.Fatal(err)
	}
	slog.New(f).Info("datagram")
	buf := make([]byte, 4096)
	pc.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, _, err := pc.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buf[:n]); !strings.HasSuffix(got, "msg=datagram") || !strings.HasPrefix(got, "<134>1 ") {
		t.Errorf("udp datagram = %q", got)
	}
}

// selfSigned makes a CA-less server certificate for 127.0.0.1/localhost
// and the client-side CAPEM that trusts it.
func selfSigned(t *testing.T) (*tls.Config, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "collector"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, _ := x509.MarshalECPrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}, string(certPEM)
}

func TestTLSRoundTripAndProbe(t *testing.T) {
	srvCfg, caPEM := selfSigned(t)
	r := newReceiver(t, srvCfg, false)
	f := newTestForwarder(t)
	cfg := tcpConfig(r)
	cfg.Transport, cfg.CAPEM = TransportTLS, caPEM
	res, err := f.Probe(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res.PeerName != "CN=collector" || res.TLSVersion == "" || res.PeerExpiry.IsZero() {
		t.Errorf("probe result = %+v", res)
	}
	if m := r.next(t); !strings.Contains(m, "event="+audit.EventForwardTest) || !strings.Contains(m, `[`+EnterpriseSDID+` event="`+audit.EventForwardTest+`" outcome="success"] `) {
		t.Errorf("probe message = %s", m)
	}
	// An untrusted collector is refused: no knob skips verification.
	bad := cfg
	bad.CAPEM = ""
	if _, err := f.Probe(context.Background(), bad); err == nil {
		t.Error("probe against an untrusted certificate succeeded")
	}
	if err := f.Apply(cfg, time.Now()); err != nil {
		t.Fatal(err)
	}
	slog.New(f).Info("over tls")
	if m := r.next(t); !strings.HasSuffix(m, `msg="over tls"`) {
		t.Errorf("tls message = %s", m)
	}
}

// --- failure handling ---

func TestOutageBacklogOrderAndRecovery(t *testing.T) {
	r := newReceiver(t, nil, false)
	f := newTestForwarder(t)
	installDefault(t, f)
	if err := f.Apply(tcpConfig(r), time.Now()); err != nil {
		t.Fatal(err)
	}
	r.next(t) // start
	l := slog.New(f)
	l.Info("before")
	last := seqOf(t, r.next(t))

	r.waitConn(t)
	r.stop() // the collector goes away: reader sees EOF
	waitState(t, f, StateDisconnected)
	for i := 0; i < 500; i++ {
		l.Info("during", "i", i)
	}
	if st := f.Status(); st.Buffered < 500 || st.DroppedTotal != 0 {
		t.Fatalf("status during outage = %+v", st)
	}

	r.restart()
	waitState(t, f, StateConnected)
	// Backlog first — disconnected record, then the 500 — in strictly
	// consecutive sequence, then the recovery record.
	first := r.next(t)
	if !strings.Contains(first, "event="+audit.EventForwardDisconnected) || seqOf(t, first) != last+1 {
		t.Fatalf("first after reconnect = %s (want disconnected, seq %d)", first, last+1)
	}
	prev := seqOf(t, first)
	for i := 0; i < 500; i++ {
		m := r.next(t)
		if s := seqOf(t, m); s != prev+1 || !strings.Contains(m, "msg=during") {
			t.Fatalf("backlog item %d: seq %d after %d: %s", i, s, prev, m)
		}
		prev++
	}
	rec := r.next(t)
	if !strings.Contains(rec, "event="+audit.EventForwardRecovered) || !strings.Contains(rec, "dropped=0") || seqOf(t, rec) != prev+1 {
		t.Errorf("recovery record = %s", rec)
	}
}

func TestOutageDropsOldestWithOneGap(t *testing.T) {
	r := newReceiver(t, nil, false)
	f := newTestForwarder(t)
	installDefault(t, f)
	alerts := &captureHandler{}
	f.c.alert = slog.New(alerts)
	if err := f.Apply(tcpConfig(r), time.Now()); err != nil {
		t.Fatal(err)
	}
	r.next(t)
	r.waitConn(t)
	r.stop()
	waitState(t, f, StateDisconnected)
	l := slog.New(f)
	extra := 500
	for i := 0; i < QueueSize+extra; i++ {
		l.Info("flood", "i", i)
	}
	st := f.Status()
	// The disconnected record occupied one slot before the flood.
	if st.Buffered != QueueSize || st.DroppedTotal != uint64(extra+1) {
		t.Fatalf("status = %+v, want full buffer and %d dropped", st, extra+1)
	}
	if !alerts.has("75%") {
		t.Error("no 75% buffer warning")
	}
	r.restart()
	waitState(t, f, StateConnected)
	first := r.next(t)
	prev := seqOf(t, first)
	// The disconnected record (seq 2) and the first 500 flood records were
	// dropped: the first surviving record is seq 2+extra+1.
	if prev != 2+extra+1 {
		t.Fatalf("first surviving seq = %d, want %d", prev, 2+extra+1)
	}
	for i := 1; i < QueueSize; i++ {
		s := seqOf(t, r.next(t))
		if s != prev+1 {
			t.Fatalf("gap inside backlog at %d -> %d", prev, s)
		}
		prev = s
	}
	rec := r.next(t)
	if !strings.Contains(rec, "event="+audit.EventForwardRecovered) || !strings.Contains(rec, "dropped="+strconv.Itoa(extra+1)) || seqOf(t, rec) != prev+1 {
		t.Errorf("recovery record = %s", rec)
	}
}

func TestIdleConnectionLossDetected(t *testing.T) {
	r := newReceiver(t, nil, false)
	f := newTestForwarder(t)
	installDefault(t, f)
	if err := f.Apply(tcpConfig(r), time.Now()); err != nil {
		t.Fatal(err)
	}
	r.next(t) // start
	waitState(t, f, StateConnected)
	r.waitConn(t)
	// Nothing queued; the collector closes the accepted connection only
	// (the listener stays up, so the reconnect that follows is immediate —
	// the records prove the loss was seen, a state read could miss it).
	r.dropConns()
	if m := r.next(t); !strings.Contains(m, "event="+audit.EventForwardDisconnected) || !strings.Contains(m, "closed by the collector") ||
		!strings.Contains(m, `[`+EnterpriseSDID+` event="`+audit.EventForwardDisconnected+`" outcome="failure"] `) {
		t.Errorf("after idle loss, first record = %s", m)
	}
	if m := r.next(t); !strings.Contains(m, "event="+audit.EventForwardRecovered) {
		t.Errorf("second record = %s", m)
	}
	if st := f.Status(); st.LastError != "" || st.State != StateConnected {
		t.Errorf("status after reconnect = %+v", st)
	}
}

func TestHaltPolicyTripsAfterTimeout(t *testing.T) {
	// A port nothing listens on.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var clock sync.Mutex
	f := New(Options{Now: func() time.Time { clock.Lock(); defer clock.Unlock(); return now }})
	t.Cleanup(func() { f.Close(context.Background()) })
	cfg := Defaults()
	cfg.Enabled, cfg.Transport, cfg.OnFailure, cfg.FailureTimeout = true, TransportTCP, OnFailureHalt, time.Minute
	h, p, _ := net.SplitHostPort(addr)
	cfg.Host = h
	cfg.Port, _ = strconv.Atoi(p)
	if err := f.Apply(cfg, time.Now()); err != nil {
		t.Fatal(err)
	}
	waitState(t, f, StateDisconnected)
	select {
	case err := <-f.Failed():
		t.Fatalf("halted before the timeout: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	clock.Lock()
	now = now.Add(61 * time.Second)
	clock.Unlock()
	select {
	case err := <-f.Failed():
		if !strings.Contains(err.Error(), "on_failure: halt") {
			t.Errorf("halt error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("halt policy did not trip")
	}
	// warn never trips.
	g := New(Options{Now: func() time.Time { clock.Lock(); defer clock.Unlock(); return now }})
	t.Cleanup(func() { g.Close(context.Background()) })
	cfg.OnFailure = OnFailureWarn
	if err := g.Apply(cfg, time.Now()); err != nil {
		t.Fatal(err)
	}
	waitState(t, g, StateDisconnected)
	clock.Lock()
	now = now.Add(time.Hour)
	clock.Unlock()
	select {
	case err := <-g.Failed():
		t.Fatalf("warn policy tripped: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestReconfigureDrainsOldAndRejectsStale(t *testing.T) {
	a := newReceiver(t, nil, false)
	b := newReceiver(t, nil, false)
	f := newTestForwarder(t)
	installDefault(t, f)
	rev1 := time.Now()
	if err := f.Apply(tcpConfig(a), rev1); err != nil {
		t.Fatal(err)
	}
	a.next(t)
	l := slog.New(f)
	l.Info("to a")
	rev2 := rev1.Add(time.Second)
	if err := f.Apply(tcpConfig(b), rev2); err != nil {
		t.Fatal(err)
	}
	if m := a.next(t); !strings.Contains(m, `msg="to a"`) {
		t.Errorf("a got %s", m)
	}
	if m := b.next(t); !strings.Contains(m, "event="+audit.EventForwardStart) {
		t.Errorf("b first = %s", m)
	}
	// Stale revision: ignored; records keep going to b.
	if err := f.Apply(tcpConfig(a), rev1); err != nil {
		t.Fatal(err)
	}
	l.Info("still b")
	if m := b.next(t); !strings.Contains(m, `msg="still b"`) {
		t.Errorf("b got %s", m)
	}
	a.none(t, 200*time.Millisecond)
	// Disabling: nothing more is forwarded, status disabled, and the stop
	// record was the last thing sent.
	off := Defaults()
	if err := f.Apply(off, rev2.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if st := f.Status(); st.State != StateDisabled {
		t.Errorf("status after disable = %+v", st)
	}
	l.Info("nowhere")
	b.none(t, 200*time.Millisecond)
	if !f.Enabled(context.Background(), slog.LevelError) {
		// disabled: Enabled is false
	} else {
		t.Error("disabled forwarder reports Enabled")
	}
}

func TestCloseSendsStopRecord(t *testing.T) {
	r := newReceiver(t, nil, false)
	f := New(Options{Hostname: "h"})
	if err := f.Apply(tcpConfig(r), time.Now()); err != nil {
		t.Fatal(err)
	}
	waitState(t, f, StateConnected)
	f.Close(context.Background())
	if m := r.next(t); !strings.Contains(m, "event="+audit.EventForwardStop) || !strings.Contains(m, `[`+EnterpriseSDID+` event="`+audit.EventForwardStop+`" outcome="success"] `) {
		t.Errorf("last message = %s", m)
	}
	if f.Status().State != StateDisabled {
		t.Error("closed forwarder not disabled")
	}
}

// TestFlappingCollectorBacksOff: a listener that accepts and immediately
// closes must not produce a connect storm (each attempt would otherwise
// also emit disconnected + recovered records).
func TestFlappingCollectorBacksOff(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var accepts int64
	var mu sync.Mutex
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			accepts++
			mu.Unlock()
			c.Close()
		}
	}()
	f := newTestForwarder(t)
	cfg := Defaults()
	cfg.Enabled, cfg.Transport = true, TransportTCP
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	cfg.Host = h
	cfg.Port, _ = strconv.Atoi(p)
	if err := f.Apply(cfg, time.Now()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(600 * time.Millisecond)
	mu.Lock()
	n := accepts
	mu.Unlock()
	// With backoff 20 → 40 → 80 → 100 ms… at most ~8 attempts fit in 600 ms;
	// without it there would be hundreds.
	if n < 2 || n > 12 {
		t.Errorf("connect attempts in 600ms = %d, want a backed-off handful", n)
	}
}

func TestApplyRejectsInvalid(t *testing.T) {
	f := newTestForwarder(t)
	c := Defaults()
	c.Enabled = true // no host
	if err := f.Apply(c, time.Now()); err == nil || !strings.Contains(err.Error(), "host") {
		t.Errorf("Apply invalid = %v", err)
	}
	if err := errors.Join(nil); err != nil {
		t.Fatal(err)
	}
}

func waitBuffered(t *testing.T, f *Forwarder, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f.Status().Buffered == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("buffered = %d, want %d", f.Status().Buffered, want)
}

func waitState(t *testing.T, f *Forwarder, want State) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f.Status().State == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("state = %s, want %s", f.Status().State, want)
}

// captureHandler records alert messages.
type captureHandler struct {
	mu   sync.Mutex
	msgs []string
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.msgs = append(h.msgs, r.Message)
	h.mu.Unlock()
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }
func (h *captureHandler) has(sub string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.msgs {
		if strings.Contains(m, sub) {
			return true
		}
	}
	return false
}

// acceptAndClose is a collector that opens and immediately drops every
// connection; count reports how many it took.
func acceptAndClose(t *testing.T, addr string) (net.Listener, func() int) {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	n := 0
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			n++
			mu.Unlock()
			c.Close()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln, func() int { mu.Lock(); defer mu.Unlock(); return n }
}

// TestHaltTripsWithFlappingCollector: a collector that accepts and drops
// before anything is written never delivers, so the outage never ends and
// halt must still trip — a connection opening is not recovery.
func TestHaltTripsWithFlappingCollector(t *testing.T) {
	ln, _ := acceptAndClose(t, "127.0.0.1:0")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var clock sync.Mutex
	f := New(Options{Now: func() time.Time { clock.Lock(); defer clock.Unlock(); return now }})
	t.Cleanup(func() { f.Close(context.Background()) })
	cfg := Defaults()
	cfg.Enabled, cfg.Transport, cfg.OnFailure, cfg.FailureTimeout = true, TransportTCP, OnFailureHalt, time.Minute
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	cfg.Host = h
	cfg.Port, _ = strconv.Atoi(p)
	if err := f.Apply(cfg, time.Now()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // a few flaps
	clock.Lock()
	now = now.Add(61 * time.Second)
	clock.Unlock()
	select {
	case err := <-f.Failed():
		if !strings.Contains(err.Error(), "on_failure: halt") {
			t.Errorf("halt error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("halt did not trip against a flapping collector")
	}
}

// TestBackoffPersistsAfterStableConnectionLost: the immediate retry earned
// by a stable connection is consumed once; a persistent outage after it
// backs off instead of retrying at full speed.
func TestBackoffPersistsAfterStableConnectionLost(t *testing.T) {
	r := newReceiver(t, nil, false)
	f := newTestForwarder(t)
	if err := f.Apply(tcpConfig(r), time.Now()); err != nil {
		t.Fatal(err)
	}
	waitState(t, f, StateConnected)
	r.waitConn(t)
	time.Sleep(stableAfter + 100*time.Millisecond) // the connection is stable
	r.stop()
	// Replace the collector with one that accepts and drops.
	var ln net.Listener
	var count func() int
	for i := 0; i < 50; i++ {
		var err error
		ln, err = net.Listen("tcp", r.addr)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if ln == nil {
		t.Fatal("could not re-listen")
	}
	ln.Close()
	ln, count = acceptAndClose(t, r.addr)
	time.Sleep(600 * time.Millisecond)
	if n := count(); n < 1 || n > 12 {
		t.Errorf("reconnect attempts in 600ms after a stable connection = %d, want a backed-off handful", n)
	}
	_ = ln
}

// TestApplyDuringOutageReportsUndelivered: replacing the destination while
// its backlog is stuck loses that backlog; the loss is alerted and carried
// into the replacement's drop total.
func TestApplyDuringOutageReportsUndelivered(t *testing.T) {
	a := newReceiver(t, nil, false)
	b := newReceiver(t, nil, false)
	f := newTestForwarder(t)
	alerts := &captureHandler{}
	f.c.alert = slog.New(alerts)
	if err := f.Apply(tcpConfig(a), time.Now()); err != nil {
		t.Fatal(err)
	}
	waitState(t, f, StateConnected)
	a.waitConn(t)
	a.stop()
	waitState(t, f, StateDisconnected)
	l := slog.New(f)
	for i := 0; i < 3; i++ {
		l.Info("stuck", "i", i)
	}
	if err := f.Apply(tcpConfig(b), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if !alerts.has("undelivered") {
		t.Error("no alert about undelivered records")
	}
	waitState(t, f, StateConnected)
	// The disconnected record plus the three stuck ones.
	if st := f.Status(); st.DroppedTotal != 4 {
		t.Errorf("replacement dropped_total = %d, want 4", st.DroppedTotal)
	}
	l.Info("to b")
	if m := b.next(t); !strings.Contains(m, `msg="to b"`) {
		t.Errorf("b got %s", m)
	}
}
