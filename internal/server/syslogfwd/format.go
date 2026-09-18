package syslogfwd

import (
	"bytes"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
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
// collector means dropped records); MSG is the record rendered as
// key=value pairs behind the UTF-8 byte-order mark RFC 5424 §6.4 requires.
type formatter struct {
	hostname string
	procID   string
	version  string
}

// bom is the UTF-8 byte order mark that prefixes MSG.
var bom = []byte{0xEF, 0xBB, 0xBF}

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

// sdEscape escapes the three characters RFC 5424 §6.3.3 forbids unescaped
// in a PARAM-VALUE.
func sdEscape(s string) string {
	if !strings.ContainsAny(s, `"\]`) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '"', '\\', ']':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
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

// message assembles one syslog message. body is the rendered MSG without
// the BOM; originIP is the local address of the current connection, or "".
func (f *formatter) message(facility int, level slog.Level, at time.Time, msgID string, seq uint32, originIP string, body []byte) []byte {
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
	b.WriteString(`"] `)
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
