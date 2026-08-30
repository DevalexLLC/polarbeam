// Package spool is the agent's crash-safe disk buffer for probe results.
//
// Spool-first single path: every result is appended to a segment file before
// it is ever pushed; the pusher reads from the head and deletes segments on
// server ack. Records are length-prefixed protobuf with a CRC32 trailer.
// Bounds are enforced by dropping oldest WHOLE segments; every drop is
// counted into a persisted lifetime total and reported to the server via
// dropped_total (plus the legacy dropped_since_last_push delta for pre-v0.4
// servers) — loss is never silent, and the total makes the server-side
// accounting idempotent under push retries.
//
// Replay is at-least-once: the in-segment read offset lives only in memory,
// so a crash replays the head segment from its start and the server's dedupe
// index absorbs the duplicates.
package spool

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
)

const (
	// Segments rotate at 1 MiB or 60 s, whichever first (fsync on rotation).
	segMaxBytes = 1 << 20
	segMaxAge   = 60 * time.Second

	// Sanity bound for a single record; a length prefix beyond this is
	// treated as corruption.
	maxRecordBytes = 1 << 20

	recordOverhead = 8 // 4-byte length prefix + 4-byte CRC32 trailer

	// droppedFile keeps its pre-v0.4 semantics — the UNACKNOWLEDGED drop
	// count, decremented on server ack — so a rolled-back agent binary
	// (docs/install.md supports downgrades) reads it exactly as it always
	// did. The lifetime total and the lifetime server-acknowledged count
	// live together in one state sidecar ("<total> <acked>") old binaries
	// ignore; a single file keeps the pair crash-atomic. Open reconciles
	// total = max(total, acked+unacked) and acked = total-unacked, then
	// persists any repair: that folds in drops an old binary made during a
	// rollback, repairs a crash between the two sidecar writes, and leaves
	// no stale ack slack for a later crash to hide a drop in.
	// Residual (accepted): drops an interim old binary makes AFTER
	// one of its pushes was acked can hide behind that ack and go
	// uncounted — bounded by the drops since the interim binary's last
	// successful push.
	droppedFile      = "dropped"
	droppedStateFile = "dropped-state"
)

type Spool struct {
	dir      string
	maxBytes int64
	maxAge   time.Duration
	now      func() time.Time // injectable for tests

	mu             sync.Mutex
	active         *os.File
	activeSeq      uint64
	activeSize     int64
	activeOpenedAt time.Time
	nextSeq        uint64

	// Read position within the head segment (memory-only; see package doc).
	readSeq    uint64
	readOffset int64

	pending        int    // appended-but-unacked records (approximate after truncation; see Next)
	droppedTotal   uint64 // lifetime total of records dropped by bounds enforcement, persisted to droppedStateFile
	droppedAcked   uint64 // lifetime count the server has acknowledged, persisted to droppedStateFile
	droppedUnacked uint64 // portion not yet acknowledged by the server, persisted to droppedFile

	// Bounds-scan trigger state. totalBytes is an approximation that only
	// ever OVERESTIMATES (appends add to it; ack-deletes and truncations
	// don't subtract), so a max_bytes breach can never be missed — it only
	// causes a scan that finds nothing to drop and resyncs the number.
	// lastBoundsCheck paces the age check; both are refreshed by every full
	// enforceBoundsLocked pass, which remains the sole authority on drops.
	totalBytes      int64
	lastBoundsCheck time.Time

	notify chan struct{}
}

// Open initializes the spool directory, scans existing segments (truncating
// any corrupt tail), loads the persisted dropped counter, and enforces the
// size/age bounds on recovered data before signaling pending work.
func Open(dir string, maxBytes int64, maxAge time.Duration) (*Spool, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("spool: %w", err)
	}
	s := &Spool{
		dir:      dir,
		maxBytes: maxBytes,
		maxAge:   maxAge,
		now:      time.Now,
		nextSeq:  1,
		notify:   make(chan struct{}, 1),
	}

	segs, err := s.listSegments()
	if err != nil {
		return nil, err
	}
	for _, seg := range segs {
		n, err := s.scanSegment(seg.path)
		if err != nil {
			return nil, err
		}
		s.pending += n
		s.nextSeq = seg.seq + 1
	}

	// Sidecar reads distinguish three cases, mirroring how segments are
	// handled: readable → use; absent or unparsable → repair loudly below
	// (like a truncated corrupt segment tail); an OPERATIONAL read error on
	// an existing file (permissions, I/O) → fatal, like any other spool I/O
	// failure — repairing over a file whose real contents we could not see
	// would erase them.
	unackedKnown := false
	if b, err := os.ReadFile(filepath.Join(dir, droppedFile)); err == nil {
		if v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64); err == nil {
			s.droppedUnacked = v
			unackedKnown = true
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("spool: read dropped counter: %w", err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, droppedStateFile)); err == nil {
		if t, a, ok := parseDroppedState(string(b)); ok {
			s.droppedTotal, s.droppedAcked = t, min(a, t)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("spool: read dropped state: %w", err)
	}
	if !unackedKnown {
		// The legacy sidecar is missing or unreadable (external damage; no
		// crash sequence of ours produces this — both sidecars are written
		// via atomic rename). On a fresh spool total is zero and this is a
		// no-op. Otherwise reconstruct the outstanding remainder from the
		// last consistent state: assuming zero would treat unreported loss
		// as acknowledged and persist that below, silently erasing it.
		s.droppedUnacked = s.droppedTotal - s.droppedAcked
		if s.droppedUnacked > 0 {
			slog.Warn("spool: unacked drop sidecar missing or unreadable, reconstructed from state",
				"unacked", s.droppedUnacked, "total", s.droppedTotal)
		}
	}
	// Reconcile (see the sidecar comment): acked+unacked is a floor on
	// lifetime loss. It exceeds the recorded total when the state sidecar
	// does not exist yet (first run after an upgrade), when an interim old
	// binary (rollback) incremented only the unacked file, or when a crash
	// landed between the two sidecar writes of a drop. It trails the total
	// only through genuine acks the state sidecar missed (an ack-crash, or
	// an interim old binary's server-recorded acks), so acked is repaired
	// to close the gap — stale-low ack progress left on disk would let a
	// later drop-crash hide inside the slack and go uncounted. Repairs are
	// persisted immediately for the same reason; failure is fatal like any
	// other spool write failure.
	t0, a0 := s.droppedTotal, s.droppedAcked
	if s.droppedUnacked > math.MaxUint64-s.droppedAcked {
		s.droppedTotal = math.MaxUint64
	} else {
		s.droppedTotal = max(s.droppedTotal, s.droppedAcked+s.droppedUnacked)
	}
	s.droppedAcked = s.droppedTotal - s.droppedUnacked
	if s.droppedTotal != t0 || s.droppedAcked != a0 || (!unackedKnown && s.droppedTotal > 0) {
		s.mu.Lock()
		err := s.persistDropCountersLocked()
		s.mu.Unlock()
		if err != nil {
			return nil, err
		}
	}
	// Recovered data must respect the same bounds as live appends: an agent
	// restarted after a long outage would otherwise replay segments older
	// than max_age (or beyond max_bytes) until the first Append prunes them.
	// Runs after the counter load so startup drops persist on top of the
	// prior total, and before the wake so pruning-to-empty signals nothing.
	s.mu.Lock()
	err = s.enforceBoundsLocked()
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if s.pending > 0 {
		s.wake()
	}
	return s, nil
}

// Append spools one result. A non-nil error is an I/O failure of the spool
// itself and is fatal for the agent: a spool that cannot be written violates
// the spool-first contract. Per-record faults — a result that cannot be
// marshalled or exceeds the record limit — are logged and counted as dropped
// (reported via dropped_since_last_push) instead of being returned: retrying
// such a record can never succeed, so it takes the same counted-loss path as
// bounds overflow instead of killing the agent. Only a failure to persist
// that drop counter — spool I/O again — surfaces as an error.
func (s *Spool) Append(res *pb.ProbeResult) error {
	payload, err := proto.Marshal(res)
	if err != nil {
		return s.dropUnspoolable(res, fmt.Errorf("marshal: %w", err))
	}
	if len(payload) > maxRecordBytes-recordOverhead {
		return s.dropUnspoolable(res, fmt.Errorf("result of %d bytes exceeds record limit", len(payload)))
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.active != nil {
		age := s.now().Sub(s.activeOpenedAt)
		if s.activeSize+int64(len(payload))+recordOverhead > segMaxBytes || age >= segMaxAge {
			if err := s.rotateLocked(); err != nil {
				return err
			}
		}
	}
	if s.active == nil {
		if err := s.openActiveLocked(); err != nil {
			return err
		}
	}

	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], uint32(len(payload)))
	if _, err := s.active.Write(buf[:]); err != nil {
		return fmt.Errorf("spool: write: %w", err)
	}
	if _, err := s.active.Write(payload); err != nil {
		return fmt.Errorf("spool: write: %w", err)
	}
	binary.LittleEndian.PutUint32(buf[:], crc32.ChecksumIEEE(payload))
	if _, err := s.active.Write(buf[:]); err != nil {
		return fmt.Errorf("spool: write: %w", err)
	}
	s.activeSize += int64(len(payload)) + recordOverhead
	s.totalBytes += int64(len(payload)) + recordOverhead
	s.pending++

	// Bounds enforcement scans the directory; paying that on every append
	// made a deep spool most expensive exactly when the agent was already
	// stressed. Scan only when the overestimating byte count says max_bytes
	// may be breached, or on a cadence comfortably inside max_age.
	if s.totalBytes > s.maxBytes || s.now().Sub(s.lastBoundsCheck) >= s.boundsCheckEvery() {
		if err := s.enforceBoundsLocked(); err != nil {
			return err
		}
	}
	s.wake()
	return nil
}

// boundsCheckEvery is the age-check cadence: frequent enough that segments
// never outlive max_age by a meaningful margin, cheap at the production
// max_age scale (days -> one scan a minute).
func (s *Spool) boundsCheckEvery() time.Duration {
	return min(time.Minute, s.maxAge/2)
}

// dropUnspoolable counts a per-record fault as a drop. A non-nil return is a
// sidecar persistence failure — spool I/O, fatal like any other Append error.
func (s *Spool) dropUnspoolable(res *pb.ProbeResult, reason error) error {
	s.mu.Lock()
	s.droppedTotal++
	s.droppedUnacked++
	err := s.persistDropCountersLocked()
	dropped := s.droppedTotal
	s.mu.Unlock()
	slog.Error("spool: dropping unspoolable result",
		"probe", res.GetProbeId(), "reason", reason, "dropped_total", dropped)
	return err
}

// Next reads up to max spooled results oldest-first without consuming them.
// The returned ack marks them consumed: fully-read sealed segments are
// deleted (fsync'd first, directory fsync'd after), a partially-read head
// advances the in-memory offset. Call ack only after the server accepted the
// batch.
func (s *Spool) Next(max int) ([]*pb.ProbeResult, func() error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	segs, err := s.listSegments()
	if err != nil {
		slog.Error("spool: listing segments failed", "err", err)
		return nil, func() error { return nil }
	}

	type consumedSeg struct {
		seq      uint64
		path     string
		end      int64
		complete bool
	}
	var out []*pb.ProbeResult
	var consumed []consumedSeg

	for _, seg := range segs {
		if len(out) >= max {
			break
		}
		start := int64(0)
		if seg.seq == s.readSeq {
			start = s.readOffset
		}
		recs, end, corrupt := s.readSegmentLocked(seg, start, max-len(out))
		if corrupt {
			s.truncateSegmentLocked(seg, end)
		}
		if len(recs) == 0 && !corrupt && seg.seq != s.activeSeq && end >= seg.size {
			// Sealed segment already fully consumed but not yet deleted
			// (e.g. a previous ack failed): clean it up on the next ack.
			consumed = append(consumed, consumedSeg{seg.seq, seg.path, end, true})
			continue
		}
		if len(recs) == 0 {
			continue
		}
		out = append(out, recs...)
		complete := end >= seg.size && seg.seq != s.activeSeq && !corrupt
		consumed = append(consumed, consumedSeg{seg.seq, seg.path, end, complete})
	}

	if len(out) == 0 {
		// Backstop for counter drift after truncations: an empty spool has
		// zero pending, whatever the arithmetic said.
		s.pending = 0
	}

	n := len(out)
	ack := func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		var errs []error
		for _, c := range consumed {
			if c.complete {
				if err := syncThenRemoveLocked(c.path); err != nil && !errors.Is(err, os.ErrNotExist) {
					errs = append(errs, err)
					continue
				}
				if s.readSeq == c.seq {
					s.readSeq, s.readOffset = 0, 0
				}
				if err := s.syncDirLocked(); err != nil {
					errs = append(errs, err)
				}
				continue
			}
			s.readSeq, s.readOffset = c.seq, c.end
			if s.active != nil && c.seq == s.activeSeq {
				if err := s.active.Sync(); err != nil {
					errs = append(errs, err)
				}
			}
		}
		s.pending = max0(s.pending - n)
		return errors.Join(errs...)
	}
	return out, ack
}

// Pending returns the approximate count of spooled-but-unacked results.
func (s *Spool) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending
}

// Dropped returns the lifetime total of results this spool has dropped and
// the portion not yet acknowledged by the server. The total goes on the wire
// as dropped_total, the unacked remainder as the legacy
// dropped_since_last_push.
func (s *Spool) Dropped() (total, unacked uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.droppedTotal, s.droppedUnacked
}

// AckDropped subtracts a successfully reported unacked amount (subtractive,
// not zeroing: drops that raced the in-flight push stay unacked) and moves
// it into the acked lifetime count. The lifetime total is never reduced.
func (s *Spool) AckDropped(reported uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reported = min(reported, s.droppedUnacked)
	s.droppedUnacked -= reported
	s.droppedAcked = min(s.droppedAcked+reported, s.droppedTotal)
	// Log-only: a persist failure here means a restart re-reports already
	// acked drops, never that loss goes uncounted. The server's idempotent
	// total absorbs the replay (a pre-v0.4 server overcounts once, its
	// previously documented behavior). Unacked before state: a crash
	// between the writes then leaves acked stale-low, which the open-time
	// reconciliation treats as nothing to repair — the reverse order would
	// inflate the total instead.
	if err := s.persistDropCountersLocked(); err != nil {
		slog.Error("spool: persisting dropped counters failed", "err", err)
	}
}

// C signals whenever new results are spooled (coalesced).
func (s *Spool) C() <-chan struct{} { return s.notify }

// Close fsyncs and closes the active segment.
func (s *Spool) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == nil {
		return nil
	}
	err := errors.Join(s.active.Sync(), s.active.Close())
	s.active = nil
	return err
}

type segment struct {
	seq  uint64
	path string
	size int64
	mod  time.Time
}

func (s *Spool) listSegments() ([]segment, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("spool: %w", err)
	}
	var segs []segment
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".seg") {
			continue
		}
		seq, err := strconv.ParseUint(strings.TrimSuffix(name, ".seg"), 10, 64)
		if err != nil {
			slog.Error("spool: ignoring alien file in spool dir", "file", name)
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		segs = append(segs, segment{seq: seq, path: filepath.Join(s.dir, name), size: info.Size(), mod: info.ModTime()})
	}
	sort.Slice(segs, func(i, j int) bool { return segs[i].seq < segs[j].seq })
	return segs, nil
}

func (s *Spool) openActiveLocked() error {
	seq := s.nextSeq
	path := filepath.Join(s.dir, fmt.Sprintf("%020d.seg", seq))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("spool: open segment: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("spool: open segment: %w", err)
	}
	s.active = f
	s.activeSeq = seq
	s.activeSize = info.Size()
	s.activeOpenedAt = s.now()
	s.nextSeq = seq + 1
	return nil
}

// rotateLocked seals the active segment: fsync, close, next Append opens a
// fresh one.
func (s *Spool) rotateLocked() error {
	if s.active == nil {
		return nil
	}
	err := errors.Join(s.active.Sync(), s.active.Close())
	s.active = nil
	if err != nil {
		return fmt.Errorf("spool: rotate: %w", err)
	}
	return nil
}

// enforceBoundsLocked drops oldest whole segments while the spool exceeds
// max_bytes or holds segments older than max_age. Dropping the active
// segment seals it first so only whole sealed segments are ever removed.
func (s *Spool) enforceBoundsLocked() error {
	for {
		segs, err := s.listSegments()
		if err != nil {
			return err
		}
		var total int64
		for _, seg := range segs {
			total += seg.size
		}
		if len(segs) == 0 {
			s.totalBytes, s.lastBoundsCheck = 0, s.now()
			return nil
		}
		oldest := segs[0]
		overSize := total > s.maxBytes
		overAge := s.now().Sub(oldest.mod) > s.maxAge
		if !overSize && !overAge {
			s.totalBytes, s.lastBoundsCheck = total, s.now()
			return nil
		}
		if oldest.seq == s.activeSeq && s.active != nil {
			if err := s.rotateLocked(); err != nil {
				return err
			}
		}
		records, _ := s.countRecords(oldest.path)
		if err := os.Remove(oldest.path); err != nil {
			return fmt.Errorf("spool: drop segment: %w", err)
		}
		if s.readSeq == oldest.seq {
			s.readSeq, s.readOffset = 0, 0
		}
		s.pending = max0(s.pending - records)
		s.droppedTotal += uint64(records)
		s.droppedUnacked += uint64(records)
		if err := s.persistDropCountersLocked(); err != nil {
			return err
		}
		reason := "max_bytes"
		if overAge {
			reason = "max_age"
		}
		slog.Error("spool overflow: dropped oldest segment",
			"segment", filepath.Base(oldest.path), "records", records,
			"reason", reason, "dropped_total", s.droppedTotal)
	}
}

// readSegmentLocked decodes up to max records starting at offset. Returns
// the records, the offset after the last good record, and whether the
// segment has a corrupt tail from that offset.
func (s *Spool) readSegmentLocked(seg segment, offset int64, max int) ([]*pb.ProbeResult, int64, bool) {
	f, err := os.Open(seg.path)
	if err != nil {
		slog.Error("spool: open for read failed", "segment", seg.path, "err", err)
		return nil, offset, false
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, true
	}

	var out []*pb.ProbeResult
	pos := offset
	var hdr [4]byte
	for len(out) < max {
		if _, err := io.ReadFull(f, hdr[:]); err != nil {
			if err == io.EOF {
				return out, pos, false // clean end
			}
			return out, pos, true // partial header: torn write
		}
		length := binary.LittleEndian.Uint32(hdr[:])
		if length > maxRecordBytes {
			return out, pos, true
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(f, payload); err != nil {
			return out, pos, true
		}
		if _, err := io.ReadFull(f, hdr[:]); err != nil {
			return out, pos, true
		}
		if binary.LittleEndian.Uint32(hdr[:]) != crc32.ChecksumIEEE(payload) {
			return out, pos, true
		}
		res := &pb.ProbeResult{}
		if err := proto.Unmarshal(payload, res); err != nil {
			return out, pos, true
		}
		out = append(out, res)
		pos += int64(length) + recordOverhead
	}
	return out, pos, false
}

// truncateSegmentLocked cuts a corrupt tail at the last good offset — loud,
// not fatal: the good prefix is preserved and replayed.
func (s *Spool) truncateSegmentLocked(seg segment, goodEnd int64) {
	lost := seg.size - goodEnd
	if seg.seq == s.activeSeq && s.active != nil {
		// Truncating the append handle's file: reopen to keep offsets sane.
		s.active.Close()
		s.active = nil
	}
	if err := os.Truncate(seg.path, goodEnd); err != nil {
		slog.Error("spool: truncating corrupt segment failed", "segment", seg.path, "err", err)
		return
	}
	slog.Error("spool: corrupt record: truncated segment tail",
		"segment", filepath.Base(seg.path), "bytes_lost", lost, "good_bytes", goodEnd)
}

// scanSegment counts records at startup, truncating any corrupt tail.
func (s *Spool) scanSegment(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("spool: %w", err)
	}
	defer f.Close()
	n, goodEnd := 0, int64(0)
	var hdr [4]byte
	for {
		if _, err := io.ReadFull(f, hdr[:]); err != nil {
			break
		}
		length := binary.LittleEndian.Uint32(hdr[:])
		if length > maxRecordBytes {
			break
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(f, payload); err != nil {
			break
		}
		if _, err := io.ReadFull(f, hdr[:]); err != nil {
			break
		}
		if binary.LittleEndian.Uint32(hdr[:]) != crc32.ChecksumIEEE(payload) {
			break
		}
		n++
		goodEnd += int64(length) + recordOverhead
	}
	if info, err := f.Stat(); err == nil && info.Size() > goodEnd {
		slog.Error("spool: corrupt record found at startup: truncating",
			"segment", filepath.Base(path), "bytes_lost", info.Size()-goodEnd)
		if err := os.Truncate(path, goodEnd); err != nil {
			return n, fmt.Errorf("spool: truncate corrupt segment: %w", err)
		}
	}
	return n, nil
}

func (s *Spool) countRecords(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	n := 0
	var hdr [4]byte
	for {
		if _, err := io.ReadFull(f, hdr[:]); err != nil {
			return n, nil
		}
		length := binary.LittleEndian.Uint32(hdr[:])
		if length > maxRecordBytes {
			return n, nil
		}
		if _, err := f.Seek(int64(length)+4, io.SeekCurrent); err != nil {
			return n, nil
		}
		n++
	}
}

// persistDropCountersLocked writes the legacy unacked sidecar BEFORE the
// state sidecar: if a crash lands between the two, the unacked file is
// ahead and the open-time reconciliation raises the total to match —
// the drop is never lost. The reverse order would leave a drop in the
// total but not in unacked, and a NULL-baseline first report would then
// skip it permanently.
func (s *Spool) persistDropCountersLocked() error {
	if err := s.persistCounterLocked(droppedFile, s.droppedUnacked); err != nil {
		return err
	}
	return s.persistFileLocked(droppedStateFile,
		strconv.FormatUint(s.droppedTotal, 10)+" "+strconv.FormatUint(s.droppedAcked, 10))
}

// parseDroppedState decodes the "<total> <acked>" state sidecar.
func parseDroppedState(b string) (total, acked uint64, ok bool) {
	fields := strings.Fields(strings.TrimSpace(b))
	if len(fields) != 2 {
		return 0, 0, false
	}
	total, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	acked, err = strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return total, acked, true
}

func (s *Spool) persistCounterLocked(name string, v uint64) error {
	return s.persistFileLocked(name, strconv.FormatUint(v, 10))
}

func (s *Spool) persistFileLocked(name, content string) error {
	path := filepath.Join(s.dir, name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return fmt.Errorf("spool: persist %s counter: %w", name, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("spool: persist %s counter: %w", name, err)
	}
	return nil
}

func (s *Spool) syncDirLocked() error {
	d, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func syncThenRemoveLocked(path string) error {
	// fsync before ack-delete: if the delete then fails, the data is intact.
	if f, err := os.OpenFile(path, os.O_RDWR, 0); err == nil {
		f.Sync()
		f.Close()
	}
	return os.Remove(path)
}

func (s *Spool) wake() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func max0(v int) int {
	if v < 0 {
		return 0
	}
	return v
}
