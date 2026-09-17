package syslogfwd

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/devalexllc/polarbeam/internal/audit"
)

// State is the forwarder's connection state as the settings page shows it.
type State string

const (
	StateDisabled     State = "disabled"
	StateConnecting   State = "connecting"
	StateConnected    State = "connected" // for UDP this means only that the socket is open
	StateDisconnected State = "disconnected"
)

// Status is a point-in-time snapshot for the API.
type Status struct {
	State        State     `json:"state"`
	Since        time.Time `json:"since"`
	Buffered     int       `json:"buffered"`
	DroppedTotal uint64    `json:"dropped_total"`
	LastError    string    `json:"last_error,omitempty"`
}

// Options configure a Forwarder.
type Options struct {
	// Version is reported as the origin swVersion.
	Version string
	// Alert receives the forwarder's own operational alerts (disconnects,
	// buffer pressure, recovery). It must NOT include the forwarder — the
	// stderr handler alone — or an outage would try to forward its own
	// alerts. nil discards them (tests).
	Alert slog.Handler
	// Hostname overrides os.Hostname() for the RFC 5424 HOSTNAME field
	// when Config.Hostname is empty (tests).
	Hostname string
	// Now is the clock (tests).
	Now func() time.Time
}

// Timing knobs, variables so tests can shorten them.
var (
	dialTimeout   = 10 * time.Second
	writeTimeout  = 10 * time.Second
	backoffMin    = time.Second
	backoffMax    = 30 * time.Second
	alertInterval = 5 * time.Minute
	drainTimeout  = 3 * time.Second
	// stableAfter is how long a connection must have lasted for the next
	// reconnect to start from backoffMin again. A collector that accepts
	// and immediately closes (a misconfigured listener, a TLS-terminating
	// proxy with no backend) must not turn into a connect storm — and a
	// storm of disconnected/recovered records.
	stableAfter = 10 * time.Second
	keepAlive   = net.KeepAliveConfig{Enable: true, Idle: 30 * time.Second, Interval: 10 * time.Second, Count: 3}
)

// Forwarder is the slog.Handler. WithAttrs/WithGroup return handlers that
// share the same core and connection; only the rendering differs.
type Forwarder struct {
	c     *core
	inner slog.Handler
}

// core is the state every derived handler shares.
type core struct {
	opts   Options
	fmt    formatter
	alert  *slog.Logger
	failed chan error

	renderMu  sync.Mutex
	renderBuf bytes.Buffer

	// seqMu orders sequence assignment with the enqueue that carries it,
	// so numbers on the wire are monotonic in enqueue order.
	seqMu sync.Mutex
	seq   uint32

	mu       sync.Mutex
	sink     *sink
	revision time.Time
	halted   bool
}

// New returns a disabled forwarder; Apply enables it.
func New(opts Options) *Forwarder {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	hostname := opts.Hostname
	if hostname == "" {
		hostname, _ = os.Hostname()
	}
	c := &core{
		opts:   opts,
		fmt:    formatter{hostname: hostname, procID: strconv.Itoa(os.Getpid()), version: opts.Version},
		failed: make(chan error, 1),
	}
	if opts.Alert != nil {
		c.alert = slog.New(opts.Alert)
	} else {
		c.alert = slog.New(slog.DiscardHandler)
	}
	inner := slog.NewTextHandler(&c.renderBuf, &slog.HandlerOptions{Level: slog.LevelDebug, ReplaceAttr: dropTimeLevel})
	return &Forwarder{c: c, inner: inner}
}

// Failed delivers the error that tripped the halt policy, once. Run
// selects on it and shuts the server down.
func (f *Forwarder) Failed() <-chan error { return f.c.failed }

// Enabled: audit records are always eligible (they are Info or Warn), so
// the gate is the lower of MinLevel and Info; operational records below
// MinLevel are dropped in Handle. A disabled forwarder accepts nothing.
func (f *Forwarder) Enabled(_ context.Context, l slog.Level) bool {
	s := f.c.current()
	if s == nil {
		return false
	}
	return l >= min(s.cfg.level(), slog.LevelInfo)
}

// Handle formats the record and queues it. It never blocks on the network
// and never returns an error to the logger: forwarding trouble is the
// writer's to report, not the caller's.
func (f *Forwarder) Handle(ctx context.Context, r slog.Record) error {
	s := f.c.current()
	if s == nil {
		return nil
	}
	isAudit := audit.IsAudit(r)
	if !isAudit && (r.Level < s.cfg.level() || s.cfg.Content == ContentAudit) {
		return nil
	}
	msgID := audit.EventID(r)
	body := f.render(ctx, r)
	// The sink read above may be retired by a concurrent Apply before the
	// push lands; a retired sink refuses it, and the record goes to the
	// sink that replaced it (or nowhere, when forwarding was disabled).
	for attempt := 0; s != nil && attempt < 3; attempt++ {
		if f.c.enqueue(s, msgID == audit.EventForwardRecovered, func(seq uint32, originIP string) []byte {
			return s.fmt.message(s.cfg.Facility, r.Level, r.Time, msgID, seq, originIP, body)
		}) {
			return nil
		}
		s = f.c.current()
	}
	return nil
}

// render turns the record into MSG through the text handler. The shared
// buffer is guarded because every derived handler writes into it.
func (f *Forwarder) render(ctx context.Context, r slog.Record) []byte {
	f.c.renderMu.Lock()
	defer f.c.renderMu.Unlock()
	f.c.renderBuf.Reset()
	_ = f.inner.Handle(ctx, r)
	return bytes.Clone(bytes.TrimRight(f.c.renderBuf.Bytes(), "\n"))
}

func (f *Forwarder) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Forwarder{c: f.c, inner: f.inner.WithAttrs(attrs)}
}

func (f *Forwarder) WithGroup(name string) slog.Handler {
	return &Forwarder{c: f.c, inner: f.inner.WithGroup(name)}
}

// Status reports the live sink's state, or disabled.
func (f *Forwarder) Status() Status {
	s := f.c.current()
	if s == nil {
		return Status{State: StateDisabled}
	}
	return s.status()
}

// Apply installs cfg as the live destination. revision is the settings
// row's updated_at: an Apply older than the one in force is ignored, so a
// late call can never regress the running sink. The previous sink drains
// for drainTimeout before the new one starts sending — records already
// queued (the settings-change record among them) reach the previous
// collector — while new records queue on the new sink meanwhile.
func (f *Forwarder) Apply(cfg Config, revision time.Time) error {
	if problems := cfg.Problems(); len(problems) > 0 {
		return errors.New(problems[0])
	}
	var next *sink
	if cfg.Enabled {
		tc, err := cfg.tlsConfig()
		if err != nil {
			return err
		}
		next = newSink(f.c, cfg, tc)
	}
	c := f.c
	c.mu.Lock()
	if !revision.IsZero() && revision.Before(c.revision) {
		c.mu.Unlock()
		return nil
	}
	prev := c.sink
	c.sink, c.revision = next, revision
	c.mu.Unlock()
	if prev != nil {
		if undelivered := prev.stop(drainTimeout); undelivered > 0 {
			// A destination replaced (or disabled) during an outage takes
			// its backlog with it. Say so, and carry the count into the
			// replacement's drop total so the status page shows it.
			c.alert.Warn("syslog destination replaced with records undelivered; they are lost",
				"undelivered", undelivered, "previous_host", prev.cfg.Host)
			if next != nil {
				next.mu.Lock()
				next.dropped += uint64(undelivered)
				next.mu.Unlock()
			}
		}
	}
	if next != nil {
		go next.run()
		audit.New(nil).Emit(context.Background(), audit.Event{
			ID: audit.EventForwardStart, Msg: "syslog forwarding started", Outcome: audit.Success,
			Attrs: []slog.Attr{
				slog.String("transport", cfg.Transport), slog.String("host", cfg.Host),
				slog.Int("port", cfg.Port), slog.String("content", cfg.Content),
				slog.String("on_failure", cfg.OnFailure),
			},
		})
	}
	return nil
}

// Close stops forwarding, draining the queue for up to ctx's deadline,
// and returns how many records were still undelivered when it gave up.
func (f *Forwarder) Close(ctx context.Context) int {
	c := f.c
	c.mu.Lock()
	s := c.sink
	c.sink = nil
	c.mu.Unlock()
	if s == nil {
		return 0
	}
	// The stop record is the last thing queued, so it is the last thing
	// the collector sees from this process.
	audit.New(nil).Emit(context.Background(), audit.Event{
		ID: audit.EventForwardStop, Msg: "syslog forwarding stopped", Outcome: audit.Success,
	})
	// Emit went to slog.Default, which no longer reaches s (sink is nil);
	// queue it on s directly.
	_ = c.enqueue(s, true, func(seq uint32, originIP string) []byte {
		return s.fmt.message(s.cfg.Facility, slog.LevelInfo, c.opts.Now(), audit.EventForwardStop, seq, originIP,
			[]byte(`msg="syslog forwarding stopped" event=`+audit.EventForwardStop+` outcome=success`))
	})
	d := drainTimeout
	if dl, ok := ctx.Deadline(); ok {
		d = time.Until(dl)
	}
	return s.stop(d)
}

func (c *core) current() *sink {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sink
}

// enqueue pushes a record onto s under the sequence lock, so sequence
// numbers are assigned in enqueue order across every producer. reserved
// marks the forwarder's own recovery/stop records, which take the spare
// slot instead of evicting a buffered record: the recovery record must
// not itself cause a drop it does not report. build may be nil to push
// nothing but a pending disconnect record. false means s is retired.
func (c *core) enqueue(s *sink, reserved bool, build func(seq uint32, originIP string) []byte) bool {
	c.seqMu.Lock()
	defer c.seqMu.Unlock()
	return s.push(reserved, build)
}

// seqNextLocked hands out the next sequence number; the caller holds seqMu.
func (c *core) seqNextLocked() uint32 {
	c.seq++
	if c.seq > 2147483647 { // RFC 5424 §7.3.1: 1..2147483647, then wrap
		c.seq = 1
	}
	return c.seq
}

// nextSeq consumes a sequence number for a message built outside enqueue
// (the probe's test message).
func (c *core) nextSeq() uint32 {
	c.seqMu.Lock()
	defer c.seqMu.Unlock()
	return c.seqNextLocked()
}

// halt trips the halt policy once.
func (c *core) halt(err error) {
	c.mu.Lock()
	first := !c.halted
	c.halted = true
	c.mu.Unlock()
	if first {
		c.alert.Error("syslog forwarding failed and on_failure is halt; stopping the server", "err", err)
		c.failed <- err
	}
}

// sink is one configured destination: its queue, its connection, and the
// goroutine that moves records from the one to the other.
type sink struct {
	c      *core
	cfg    Config
	tlsCfg *tls.Config
	// fmt is the core formatter with the configured HOSTNAME applied.
	fmt formatter

	mu   sync.Mutex
	cond *sync.Cond
	ring [][]byte
	head int
	n    int

	conn        net.Conn
	connectedAt time.Time
	localIP     string
	state       State
	since       time.Time
	lastErr     string
	stopping    bool
	stopCh      chan struct{} // closed by stop; wakes a backoff sleep
	done        chan struct{}
	// ctx is cancelled by stop so a dial in flight returns at once.
	ctx    context.Context
	cancel context.CancelFunc

	dropped         uint64
	droppedAtOutage uint64
	// firstFailure marks an outage from its first failure until a record
	// is DELIVERED again — a connection that opens is not recovery.
	firstFailure time.Time
	lastAlert    time.Time
	warned75     bool
	// attempted is set once the first dial has been made, so only the
	// very first connect goes without a backoff sleep.
	attempted bool
	// pendingDisconnect is the outage's disconnected record, queued by the
	// next push under the sequence lock so it precedes every record
	// produced after the failure became observable.
	pendingDisconnect error
	// resetBackoff is set when a lost connection had been stable; the
	// writer consumes it once.
	resetBackoff bool
}

func newSink(c *core, cfg Config, tc *tls.Config) *sink {
	ctx, cancel := context.WithCancel(context.Background())
	s := &sink{c: c, cfg: cfg, tlsCfg: tc, fmt: c.formatterFor(cfg), ring: make([][]byte, QueueSize+1), state: StateConnecting, since: c.opts.Now(), stopCh: make(chan struct{}), done: make(chan struct{}), ctx: ctx, cancel: cancel}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// formatterFor applies the configured HOSTNAME override, if any.
func (c *core) formatterFor(cfg Config) formatter {
	f := c.fmt
	if cfg.Hostname != "" {
		f.hostname = cfg.Hostname
	}
	return f
}

func (s *sink) originIP() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.localIP
}

func (s *sink) status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Status{State: s.state, Since: s.since, Buffered: s.n, DroppedTotal: s.dropped, LastError: s.lastErr}
}

// push appends to the ring (the caller holds the sequence lock; sequence
// numbers are assigned here so a pending disconnect record takes the
// number just before the record that follows it), dropping and counting
// the oldest record when the QueueSize budget is spent, and warns once
// per fill cycle at 75 % (ASD STIG V-222483). The ring has one spare slot
// beyond QueueSize that only a reserved record (recovery, stop) may
// occupy, so those never evict what they are about to account for. A
// retired sink refuses the push.
func (s *sink) push(reserved bool, build func(seq uint32, originIP string) []byte) bool {
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		return false
	}
	if err := s.pendingDisconnect; err != nil {
		s.pendingDisconnect = nil
		body := []byte(`msg="syslog forwarding disconnected" event=` + audit.EventForwardDisconnected +
			` outcome=failure host=` + quoteValue(s.cfg.Host) + ` reason=` + quoteValue(err.Error()))
		s.appendLocked(s.fmt.message(s.cfg.Facility, slog.LevelWarn, s.c.opts.Now(), audit.EventForwardDisconnected,
			s.c.seqNextLocked(), s.localIP, body), false)
	}
	if build != nil {
		s.appendLocked(build(s.c.seqNextLocked(), s.localIP), reserved)
	}
	warn := !s.warned75 && s.n*4 >= QueueSize*3
	if warn {
		s.warned75 = true
	}
	n, dropped := s.n, s.dropped
	s.cond.Signal()
	s.mu.Unlock()
	if warn {
		s.c.alert.Warn("syslog forwarding buffer is 75% full; records will be dropped when it fills",
			"buffered", n, "capacity", QueueSize, "dropped_total", dropped)
	}
	return true
}

// appendLocked adds one message to the ring; the caller holds s.mu.
func (s *sink) appendLocked(msg []byte, reserved bool) {
	if s.n >= QueueSize && !(reserved && s.n == QueueSize) {
		s.head = (s.head + 1) % len(s.ring)
		s.n--
		s.dropped++
	}
	s.ring[(s.head+s.n)%len(s.ring)] = msg
	s.n++
}

// quoteValue renders a value the way slog's text handler would.
func quoteValue(v string) string {
	if v == "" || strings.ContainsAny(v, " \t\n\"=") {
		return strconv.Quote(v)
	}
	return v
}

func (s *sink) pop() {
	s.mu.Lock()
	if s.n > 0 {
		s.ring[s.head] = nil
		s.head = (s.head + 1) % len(s.ring)
		s.n--
		if s.n*2 < QueueSize {
			s.warned75 = false
		}
	}
	s.mu.Unlock()
}

// run is the writer goroutine: connect (with backoff) while disconnected,
// otherwise wait for records and write them one at a time. A write
// failure, a connect failure, or the reader noticing the peer went away
// all funnel through fail, which owns the alerting and the halt policy.
func (s *sink) run() {
	defer close(s.done)
	backoff := backoffMin
	for {
		s.mu.Lock()
		for !s.stopping && s.conn != nil && s.n == 0 {
			s.cond.Wait()
		}
		if s.stopping && (s.n == 0 || s.conn == nil) {
			conn := s.conn
			s.conn = nil
			s.mu.Unlock()
			if conn != nil {
				conn.Close()
			}
			return
		}
		if s.conn == nil {
			// Back off before every reconnect except the one right after a
			// stable connection was lost (consumed once): a dial that
			// failed and a connection that died young both wait, longer
			// each time, so a flapping collector is not a connect storm.
			reset := s.resetBackoff
			s.resetBackoff = false
			attempted := s.attempted
			s.attempted = true
			s.mu.Unlock()
			switch {
			case reset:
				backoff = backoffMin
			case attempted:
				if !s.sleep(backoff) {
					return
				}
				backoff = min(backoff*2, backoffMax)
			}
			conn, err := s.dial()
			if err != nil {
				s.fail(nil, err)
				continue
			}
			s.connected(conn)
			continue
		}
		msg := s.ring[s.head]
		conn := s.conn
		s.mu.Unlock()
		_ = conn.SetWriteDeadline(s.c.opts.Now().Add(writeTimeout))
		if _, err := conn.Write(frame(msg, s.cfg.Transport, s.cfg.Framing)); err != nil {
			s.fail(conn, err)
			continue
		}
		s.pop()
		s.delivered()
	}
}

// delivered runs after a successful write: if an outage was in progress,
// this is its end — not the reconnect, which a collector can accept and
// drop before anything is written. Recovery is recorded (queued behind
// whatever is still buffered) and the halt timer is cleared.
func (s *sink) delivered() {
	now := s.c.opts.Now()
	s.mu.Lock()
	if s.firstFailure.IsZero() {
		s.mu.Unlock()
		return
	}
	outage := now.Sub(s.firstFailure)
	droppedDuring := s.dropped - s.droppedAtOutage
	s.firstFailure = time.Time{}
	s.mu.Unlock()
	s.c.alert.Info("syslog collector reachable again", "host", s.cfg.Host, "outage", outage, "dropped", droppedDuring)
	audit.New(nil).Emit(context.Background(), audit.Event{
		ID: audit.EventForwardRecovered, Msg: "syslog forwarding recovered", Outcome: audit.Success,
		Attrs: []slog.Attr{slog.Duration("outage", outage), slog.Uint64("dropped", droppedDuring)},
	})
}

// sleep waits d or until stop; false means stop.
func (s *sink) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-s.stopCh:
		return false
	}
}

// dial opens the transport with TCP keepalives on (a silently vanished
// peer is detected within roughly a minute even with nothing to send).
func (s *sink) dial() (net.Conn, error) {
	ctx, cancel := context.WithTimeout(s.ctx, dialTimeout)
	defer cancel()
	d := &net.Dialer{Timeout: dialTimeout, KeepAliveConfig: keepAlive}
	switch s.cfg.Transport {
	case TransportUDP:
		return d.DialContext(ctx, "udp", s.cfg.addr())
	case TransportTCP:
		return d.DialContext(ctx, "tcp", s.cfg.addr())
	default:
		td := &tls.Dialer{NetDialer: d, Config: s.tlsCfg}
		return td.DialContext(ctx, "tcp", s.cfg.addr())
	}
}

// connected installs the connection, records a recovery if this ends an
// outage, and starts the reader that detects the peer going away.
func (s *sink) connected(conn net.Conn) {
	now := s.c.opts.Now()
	s.mu.Lock()
	s.conn, s.connectedAt = conn, now
	if host, _, err := net.SplitHostPort(conn.LocalAddr().String()); err == nil {
		s.localIP = host
	}
	s.state, s.since, s.lastErr = StateConnected, now, ""
	s.mu.Unlock()
	if s.cfg.Transport != TransportUDP {
		go s.watch(conn)
	}
}

// watch blocks reading the connection. Syslog collectors never send
// application data, so any return — EOF on a graceful close, a reset, or
// the kernel's keepalive verdict — means the peer is gone.
func (s *sink) watch(conn net.Conn) {
	_, err := io.Copy(io.Discard, conn)
	if err == nil {
		err = errors.New("connection closed by the collector")
	}
	s.fail(conn, err)
}

// fail records a lost connection (conn nil for a failed dial). The first
// failure of an outage alerts immediately and queues a disconnected
// record; later ones alert every alertInterval; the halt policy trips
// once FailureTimeout has elapsed since the first failure.
func (s *sink) fail(conn net.Conn, err error) {
	now := s.c.opts.Now()
	s.mu.Lock()
	if s.stopping || (conn != nil && s.conn != conn) {
		s.mu.Unlock()
		return
	}
	if conn != nil {
		s.conn = nil
		conn.Close()
		// A connection that had been up for a while earns the next
		// reconnect an immediate try; a short-lived one does not.
		s.resetBackoff = now.Sub(s.connectedAt) >= stableAfter
		s.connectedAt = time.Time{}
	}
	first := s.firstFailure.IsZero()
	if first {
		s.firstFailure, s.droppedAtOutage, s.lastAlert = now, s.dropped, now
		s.pendingDisconnect = err
	}
	s.state, s.since, s.lastErr = StateDisconnected, s.firstFailure, err.Error()
	periodic := !first && now.Sub(s.lastAlert) >= alertInterval
	if periodic {
		s.lastAlert = now
	}
	down := now.Sub(s.firstFailure)
	n, dropped := s.n, s.dropped-s.droppedAtOutage
	halt := s.cfg.OnFailure == OnFailureHalt && down >= s.cfg.FailureTimeout
	s.cond.Broadcast()
	s.mu.Unlock()
	switch {
	case first:
		s.c.alert.Error("syslog collector unreachable; buffering records", "host", s.cfg.Host, "port", s.cfg.Port, "err", err)
		// The queued copy is inserted by the next push, under the sequence
		// lock, so it precedes every record produced after the failure
		// became observable; this push makes sure it is queued even when
		// nothing else is produced. The stderr copy goes straight to the
		// alert handler.
		s.c.enqueue(s, true, nil)
		audit.New(s.c.alert).Emit(context.Background(), audit.Event{
			ID: audit.EventForwardDisconnected, Msg: "syslog forwarding disconnected", Outcome: audit.Failure,
			Attrs: []slog.Attr{slog.String("host", s.cfg.Host), slog.String("reason", err.Error())},
		})
	case periodic:
		s.c.alert.Error("syslog collector still unreachable", "host", s.cfg.Host, "down_for", down,
			"buffered", n, "dropped", dropped, "err", err)
	}
	if halt {
		s.c.halt(fmt.Errorf("syslog collector %s unreachable for %s (on_failure: halt): %w", s.cfg.addr(), down.Truncate(time.Second), err))
	}
}

// stop drains for up to d, then closes; it returns the records left
// undelivered.
func (s *sink) stop(d time.Duration) int {
	s.mu.Lock()
	if !s.stopping {
		s.stopping = true
		close(s.stopCh)
		s.cancel() // a dial in flight returns now; a live connection drains
	}
	s.cond.Broadcast()
	s.mu.Unlock()
	select {
	case <-s.done:
	case <-time.After(d):
		s.mu.Lock()
		conn := s.conn
		s.conn = nil
		s.mu.Unlock()
		if conn != nil {
			conn.Close() // unblocks a stalled write; run then exits
		}
		<-s.done
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

// ProbeResult is what a successful test connection learned.
type ProbeResult struct {
	Transport  string    `json:"transport"`
	Addr       string    `json:"addr"`
	LocalAddr  string    `json:"local_addr"`
	TLSVersion string    `json:"tls_version,omitempty"`
	PeerName   string    `json:"peer_subject,omitempty"`
	PeerExpiry time.Time `json:"peer_not_after,omitempty"`
}

// Probe dials cfg, completes the handshake, sends one test record, and
// closes — the Test button and the startup preflight. It never touches
// the live sink.
func (f *Forwarder) Probe(ctx context.Context, cfg Config) (*ProbeResult, error) {
	if problems := cfg.Problems(); len(problems) > 0 {
		return nil, errors.New(problems[0])
	}
	tc, err := cfg.tlsConfig()
	if err != nil {
		return nil, err
	}
	s := &sink{c: f.c, cfg: cfg, tlsCfg: tc, fmt: f.c.formatterFor(cfg), ctx: ctx}
	conn, err := s.dial()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	deadline := f.c.opts.Now().Add(writeTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	res := &ProbeResult{Transport: cfg.Transport, Addr: cfg.addr(), LocalAddr: conn.LocalAddr().String()}
	if tconn, ok := conn.(*tls.Conn); ok {
		st := tconn.ConnectionState()
		res.TLSVersion = tls.VersionName(st.Version)
		if len(st.PeerCertificates) > 0 {
			res.PeerName = st.PeerCertificates[0].Subject.String()
			res.PeerExpiry = st.PeerCertificates[0].NotAfter
		}
	}
	originIP := ""
	if host, _, err := net.SplitHostPort(conn.LocalAddr().String()); err == nil {
		originIP = host
	}
	msg := s.fmt.message(cfg.Facility, slog.LevelInfo, f.c.opts.Now(), audit.EventForwardTest, f.c.nextSeq(), originIP,
		[]byte(`msg="syslog forwarding test" event=`+audit.EventForwardTest+` outcome=success`))
	_ = conn.SetWriteDeadline(deadline)
	if _, err := conn.Write(frame(msg, cfg.Transport, cfg.Framing)); err != nil {
		return nil, fmt.Errorf("send test record: %w", err)
	}
	return res, nil
}
