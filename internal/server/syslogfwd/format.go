package syslogfwd

import (
	"bytes"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/devalexllc/polarbeam/internal/audit"
)

// formatter builds RFC 5424 messages:
//
//	<PRI>1 TIMESTAMP HOSTNAME APP-NAME PROCID MSGID SD BOM MSG
//
// PRI is facility×8+severity; TIMESTAMP is RFC 3339 UTC with microseconds;
// MSGID is the audit event id (NILVALUE for an operational record); SD is
// the three IANA-registered elements — timeQuality (the timestamp carries
// its offset), origin (software, version, and the socket's local address
// once known), and meta (a per-process sequence number so a gap on the
// collector means dropped records) — followed, on an audit record, by the
// enterprise element EnterpriseSDID carrying the fixed audit fields; MSG
// is the record rendered as key=value pairs behind the UTF-8 byte-order
// mark RFC 5424 §6.4 requires.
type formatter struct {
	hostname string
	procID   string
	version  string
}

// bom is the UTF-8 byte order mark that prefixes MSG.
var bom = []byte{0xEF, 0xBB, 0xBF}

// EnterpriseSDID is the SD-ID of PolarBEAM's own structured-data element
// (RFC 5424 §6.3.2): a name of our choosing at Devalex LLC's IANA Private
// Enterprise Number, 66894. The element duplicates the fixed audit fields
// (audit.KeyEvent and the keys Emit writes after it) as SD-PARAMs so a
// collector can index them without parsing MSG; MSG keeps every field.
const EnterpriseSDID = "polarbeam@66894"

// sdParam is one PARAM-NAME="PARAM-VALUE" of the enterprise element.
type sdParam struct {
	name, value string
}

// sdParamKeys are the record attributes copied into the enterprise
// element, in the order audit.Emit writes them. Only the first attribute
// under each key counts: an event-specific attribute cannot shadow a
// fixed one.
var sdParamKeys = [...]string{audit.KeyEvent, audit.KeyOutcome, audit.KeyUser, audit.KeyUserSource, audit.KeySession, audit.KeyRemote}

// maxSDParamValue bounds one PARAM-VALUE. RFC 5424 sets no limit, but the
// element precedes MSG and truncation cuts MSG, so a runaway value must
// not eat the record it describes. Fixed audit fields are identifiers
// (event ids, UUIDs, usernames, host:port); real values are far shorter.
const maxSDParamValue = 255

// auditParams extracts the enterprise element's parameters from a record:
// nil for an operational record (no element is emitted), the present fixed
// fields in Emit order for an audit record.
func auditParams(r slog.Record) []sdParam {
	if !audit.IsAudit(r) {
		return nil
	}
	var vals [len(sdParamKeys)]string
	var seen [len(sdParamKeys)]bool
	r.Attrs(func(a slog.Attr) bool {
		for i, k := range sdParamKeys {
			if a.Key == k && !seen[i] {
				seen[i] = true
				vals[i] = a.Value.String()
			}
		}
		return true
	})
	out := make([]sdParam, 0, len(sdParamKeys))
	for i, k := range sdParamKeys {
		if seen[i] {
			out = append(out, sdParam{k, vals[i]})
		}
	}
	return out
}

// eventParams is the element for a record the forwarder synthesizes itself
// (its own lifecycle events): the event id and a success outcome.
func eventParams(id string, outcome audit.Outcome) []sdParam {
	return []sdParam{{audit.KeyEvent, id}, {audit.KeyOutcome, string(outcome)}}
}

// clipRunes bounds s to max bytes on a UTF-8 rune boundary, so a clipped
// PARAM-VALUE stays valid UTF-8 as RFC 5424 §6.3.3 requires.
func clipRunes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// severity maps slog levels onto RFC 5424 §6.2.1 severities.
func severity(l slog.Level) int {
	switch {
	case l >= slog.LevelError:
		return 3
	case l >= slog.LevelWarn:
		return 4
	case l >= slog.LevelInfo:
		return 6
	}
	return 7
}

// sdEscape prepares a PARAM-VALUE. Control characters (C0 and DEL) and
// '%' are percent-encoded first (%0A, %25), then the three characters RFC
// 5424 §6.3.3 forbids unescaped are backslash-escaped. The header must
// never carry a raw LF — non-transparent framing ends a record on it —
// and PARAM-VALUEs carry caller-supplied text (a claimed username on a
// failed login), so this is a security boundary. The two layers are
// separate because a receiver's RFC decoding folds `\\` to `\`: a
// backslash spelling of controls would make "a\nb" and `a\nb` decode to
// the same indexed value. Percent-encoding survives that decoding and
// stays reversible: RFC-unescape, then percent-decode.
func sdEscape(s string) string {
	if !strings.ContainsAny(s, `"\]%`) && !hasControl(s) {
		return s
	}
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\\' || c == ']':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20 || c == 0x7f || c == '%':
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xf])
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// hasControl reports whether s contains a C0 control character or DEL.
func hasControl(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}

// nilIfEmpty is the NILVALUE for an absent header field.
func nilIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// truncateAt returns "-" when the string exceeds max — RFC 5424 bounds
// APP-NAME (48), PROCID (128), MSGID (32), HOSTNAME (255), and the origin
// swVersion parameter (32).
func clip(s string, max int) string {
	if len(s) > max {
		return s[:max]
	}
	return s
}

// message assembles one syslog message. params are the enterprise
// element's SD-PARAMs (nil: no element, an operational record); body is
// the rendered MSG without the BOM; originIP is the local address of the
// current connection, or "".
func (f *formatter) message(facility int, level slog.Level, at time.Time, msgID string, seq uint32, originIP string, params []sdParam, body []byte) []byte {
	var b bytes.Buffer
	b.Grow(256 + len(body))
	b.WriteByte('<')
	b.WriteString(strconv.Itoa(facility*8 + severity(level)))
	b.WriteString(">1 ")
	b.WriteString(at.UTC().Format("2006-01-02T15:04:05.000000Z"))
	b.WriteByte(' ')
	b.WriteString(nilIfEmpty(clip(f.hostname, 255)))
	b.WriteByte(' ')
	b.WriteString(clip(AppName, 48))
	b.WriteByte(' ')
	b.WriteString(nilIfEmpty(clip(f.procID, 128)))
	b.WriteByte(' ')
	b.WriteString(nilIfEmpty(clip(msgID, 32)))
	b.WriteString(` [timeQuality tzKnown="1"][origin software="`)
	b.WriteString(sdEscape(AppName))
	b.WriteString(`" swVersion="`)
	b.WriteString(sdEscape(clip(f.version, 32)))
	b.WriteByte('"')
	if originIP != "" {
		b.WriteString(` ip="`)
		b.WriteString(sdEscape(originIP))
		b.WriteByte('"')
	}
	b.WriteString(`][meta sequenceId="`)
	b.WriteString(strconv.FormatUint(uint64(seq), 10))
	b.WriteString(`"]`)
	if len(params) > 0 {
		b.WriteByte('[')
		b.WriteString(EnterpriseSDID)
		for _, p := range params {
			b.WriteByte(' ')
			b.WriteString(p.name)
			b.WriteString(`="`)
			b.WriteString(sdEscape(clipRunes(p.value, maxSDParamValue)))
			b.WriteByte('"')
		}
		b.WriteByte(']')
	}
	b.WriteByte(' ')
	b.Write(bom)
	b.Write(body)
	return truncate(b.Bytes())
}

// truncatedMark replaces the tail of an over-long message so the cut is
// visible to whoever reads the record.
const truncatedMark = " truncated=1"

// truncate enforces MaxMessageBytes at the end of MSG (RFC 5424 §6.1),
// cutting on a UTF-8 rune boundary so the message stays valid UTF-8.
func truncate(msg []byte) []byte {
	if len(msg) <= MaxMessageBytes {
		return msg
	}
	cut := MaxMessageBytes - len(truncatedMark)
	for cut > 0 && !utf8.RuneStart(msg[cut]) {
		cut--
	}
	out := make([]byte, 0, MaxMessageBytes)
	out = append(out, msg[:cut]...)
	return append(out, truncatedMark...)
}

// frame applies the transport's framing: octet counting prefixes the
// length (RFC 6587 §3.4.1, RFC 5425 §4.3), non-transparent framing appends
// LF (RFC 6587 §3.4.2), UDP sends the bare message (RFC 5426).
func frame(msg []byte, transport, framing string) []byte {
	switch {
	case transport == TransportUDP:
		return msg
	case framing == FramingNonTransparent:
		return append(msg, '\n')
	default:
		n := strconv.Itoa(len(msg))
		out := make([]byte, 0, len(n)+1+len(msg))
		out = append(out, n...)
		out = append(out, ' ')
		return append(out, msg...)
	}
}

// renderBody turns a record into MSG through a slog.TextHandler whose
// time and level attributes are dropped (they are in the header): the
// result is `msg="..." key=value ...` with slog's quoting, which never
// contains a raw newline — so non-transparent framing is safe.
func dropTimeLevel(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && (a.Key == slog.TimeKey || a.Key == slog.LevelKey) {
		return slog.Attr{}
	}
	return a
}
