package spool

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
)

func result(i int) *pb.ProbeResult {
	return &pb.ProbeResult{
		ProbeId:   fmt.Sprintf("probe-%06d", i),
		Type:      pb.ProbeType_PROBE_TYPE_TCP,
		TargetId:  "target",
		StartedAt: timestamppb.New(time.Unix(int64(1700000000+i), 0)),
		Status:    pb.ProbeStatus_PROBE_STATUS_OK,
		JitterUs:  -1,
	}
}

func mustOpen(t *testing.T, dir string, maxBytes int64, maxAge time.Duration) *Spool {
	t.Helper()
	s, err := Open(dir, maxBytes, maxAge)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func appendN(t *testing.T, s *Spool, from, n int) {
	t.Helper()
	for i := from; i < from+n; i++ {
		if err := s.Append(result(i)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRoundtripPreservesOrder(t *testing.T) {
	s := mustOpen(t, t.TempDir(), 1<<30, time.Hour)
	appendN(t, s, 0, 10)

	got, ack := s.Next(100)
	if len(got) != 10 {
		t.Fatalf("got %d results, want 10", len(got))
	}
	for i, r := range got {
		if r.ProbeId != fmt.Sprintf("probe-%06d", i) {
			t.Errorf("result %d out of order: %s", i, r.ProbeId)
		}
	}
	if err := ack(); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Next(100); len(got) != 0 {
		t.Errorf("acked results replayed: %d", len(got))
	}
	if s.Pending() != 0 {
		t.Errorf("pending = %d after full ack", s.Pending())
	}
}

func TestUnackedResultsReplay(t *testing.T) {
	s := mustOpen(t, t.TempDir(), 1<<30, time.Hour)
	appendN(t, s, 0, 5)

	got, _ := s.Next(100) // no ack: push failed
	if len(got) != 5 {
		t.Fatalf("got %d", len(got))
	}
	again, _ := s.Next(100)
	if len(again) != 5 {
		t.Errorf("unacked batch must replay in full, got %d", len(again))
	}
}

func TestPartialAckAdvances(t *testing.T) {
	s := mustOpen(t, t.TempDir(), 1<<30, time.Hour)
	appendN(t, s, 0, 10)

	got, ack := s.Next(4)
	if len(got) != 4 {
		t.Fatalf("got %d, want 4", len(got))
	}
	if err := ack(); err != nil {
		t.Fatal(err)
	}
	rest, ack2 := s.Next(100)
	if len(rest) != 6 {
		t.Fatalf("got %d remaining, want 6", len(rest))
	}
	if rest[0].ProbeId != "probe-000004" {
		t.Errorf("resumed at %s, want probe-000004", rest[0].ProbeId)
	}
	if err := ack2(); err != nil {
		t.Fatal(err)
	}
}

func TestRotationBySize(t *testing.T) {
	dir := t.TempDir()
	s := mustOpen(t, dir, 1<<30, time.Hour)
	// Push well past 1 MiB so at least one rotation happens.
	big := result(0)
	big.Error = string(make([]byte, 8192))
	for range 200 {
		if err := s.Append(big); err != nil {
			t.Fatal(err)
		}
	}
	segs, err := s.listSegments()
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) < 2 {
		t.Fatalf("expected rotation to produce multiple segments, got %d", len(segs))
	}
	for _, seg := range segs {
		if seg.size > segMaxBytes+16384 {
			t.Errorf("segment %s is %d bytes, exceeds rotation threshold", seg.path, seg.size)
		}
	}
}

func TestRotationByAge(t *testing.T) {
	s := mustOpen(t, t.TempDir(), 1<<30, time.Hour)
	now := time.Now()
	s.now = func() time.Time { return now }
	appendN(t, s, 0, 1)
	// Advance the injected clock past the segment age limit.
	now = now.Add(segMaxAge + time.Second)
	appendN(t, s, 1, 1)
	segs, _ := s.listSegments()
	if len(segs) != 2 {
		t.Fatalf("expected age rotation to seal the first segment, got %d segments", len(segs))
	}
}

func TestCrashReplayFromSegmentStart(t *testing.T) {
	dir := t.TempDir()
	s := mustOpen(t, dir, 1<<30, time.Hour)
	appendN(t, s, 0, 8)
	got, ack := s.Next(4)
	if len(got) != 4 {
		t.Fatal("short read")
	}
	if err := ack(); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// "Crash": reopen. The in-memory offset is gone, the whole head segment
	// replays (at-least-once; server dedupes).
	s2 := mustOpen(t, dir, 1<<30, time.Hour)
	if s2.Pending() != 8 {
		t.Errorf("pending after reopen = %d, want 8 (full segment)", s2.Pending())
	}
	replayed, _ := s2.Next(100)
	if len(replayed) != 8 {
		t.Errorf("replayed %d, want 8", len(replayed))
	}
}

func TestCorruptTailTruncated(t *testing.T) {
	dir := t.TempDir()
	s := mustOpen(t, dir, 1<<30, time.Hour)
	appendN(t, s, 0, 3)
	s.Close()

	segs, _ := s.listSegments()
	if len(segs) != 1 {
		t.Fatal("expected one segment")
	}
	// Corrupt the last record's CRC.
	f, err := os.OpenFile(segs[0].path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{0xff, 0xff, 0xff, 0xff}, segs[0].size-4); err != nil {
		t.Fatal(err)
	}
	f.Close()

	s2 := mustOpen(t, dir, 1<<30, time.Hour)
	if s2.Pending() != 2 {
		t.Errorf("pending = %d, want 2 (corrupt record dropped)", s2.Pending())
	}
	got, _ := s2.Next(100)
	if len(got) != 2 {
		t.Errorf("read %d, want 2", len(got))
	}
}

func TestOverflowDropsOldestAndCounts(t *testing.T) {
	dir := t.TempDir()
	// Tiny bound (< one segment) exercises the seal-active-then-drop path,
	// same as the dev-stack overflow scenario.
	s := mustOpen(t, dir, 8192, time.Hour)
	big := result(0)
	big.Error = string(make([]byte, 1024))
	for i := range 30 {
		big.ProbeId = fmt.Sprintf("probe-%06d", i)
		if err := s.Append(big); err != nil {
			t.Fatal(err)
		}
	}
	if total, _ := s.Dropped(); total == 0 {
		t.Fatal("overflow must increment the dropped counter")
	}
	var total int64
	segs, _ := s.listSegments()
	for _, seg := range segs {
		total += seg.size
	}
	if total > 8192+2048 {
		t.Errorf("spool size %d not bounded near max_bytes", total)
	}

	// Both counters survive a restart via the sidecar files.
	droppedBefore, _ := s.Dropped()
	s.Close()
	s2 := mustOpen(t, dir, 8192, time.Hour)
	if total, unacked := s2.Dropped(); total != droppedBefore || unacked != droppedBefore {
		t.Errorf("counters wrong across reopen: total=%d unacked=%d, want both %d",
			total, unacked, droppedBefore)
	}

	// A server ack zeroes the unacked remainder, never the lifetime total.
	// The unacked sidecar keeps its pre-v0.4 semantics (rollback safety),
	// so after a full ack it reads 0.
	s2.AckDropped(droppedBefore)
	if total, unacked := s2.Dropped(); total != droppedBefore || unacked != 0 {
		t.Errorf("after ack: total=%d unacked=%d, want total=%d unacked=0",
			total, unacked, droppedBefore)
	}
	if b, err := os.ReadFile(filepath.Join(dir, droppedFile)); err != nil || string(b) != "0" {
		t.Errorf("unacked sidecar not persisted after ack: %q, %v", b, err)
	}
	wantState := strconv.FormatUint(droppedBefore, 10) + " " + strconv.FormatUint(droppedBefore, 10)
	if b, err := os.ReadFile(filepath.Join(dir, droppedStateFile)); err != nil || string(b) != wantState {
		t.Errorf("state sidecar after ack: %q, %v, want %q", b, err, wantState)
	}

	// Both counters survive another restart.
	s2.Close()
	s3 := mustOpen(t, dir, 8192, time.Hour)
	if total, unacked := s3.Dropped(); total != droppedBefore || unacked != 0 {
		t.Errorf("counters lost across reopen after ack: total=%d unacked=%d", total, unacked)
	}
}

func TestDroppedCounterReconciliation(t *testing.T) {
	// Open reconciles total = max(total, acked+unacked): acked+unacked is a
	// floor on lifetime loss whenever the state sidecar trails the legacy
	// unacked one.
	tests := []struct {
		name           string
		unackedFile    string // legacy "dropped" sidecar; "" = absent
		stateFile      string // "<total> <acked>" sidecar; "" = absent
		total, unacked uint64
	}{
		// First run after an upgrade: only the pre-v0.4 sidecar exists.
		{"upgrade from pre-v0.4", "5", "", 5, 5},
		// Rollback: an interim old binary added 5 unacked drops the state
		// sidecar knows nothing about — they must raise the total or a
		// baseline-100 server would never count them.
		{"rollback-era drops", "5", "100 100", 105, 5},
		// Crash between the unacked write and the state write: unacked is
		// ahead by one; reconciliation repairs the total.
		{"crash between sidecar writes", "1", "0 0", 1, 1},
		// Steady state: nothing to repair.
		{"consistent", "2", "9 7", 9, 2},
		// Corrupt state sidecar is ignored, unacked still floors the total.
		{"corrupt state file", "4", "not numbers", 4, 4},
		// Acked beyond total (desynced) clamps rather than inflating.
		{"acked clamped to total", "0", "3 8", 3, 0},
		// Ack-crash remnant: unacked was cleared but the state write was
		// lost. The acks were real, so acked is repaired to total-unacked.
		{"ack-crash repairs acked", "0", "100 95", 100, 0},
		// Legacy sidecar lost or damaged externally: the outstanding
		// remainder is reconstructed from the state, never assumed acked.
		{"missing unacked file", "", "100 95", 100, 5},
		{"malformed unacked file", "junk", "100 95", 100, 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.unackedFile != "" {
				os.WriteFile(filepath.Join(dir, droppedFile), []byte(tt.unackedFile), 0o600)
			}
			if tt.stateFile != "" {
				os.WriteFile(filepath.Join(dir, droppedStateFile), []byte(tt.stateFile), 0o600)
			}
			s := mustOpen(t, dir, 1<<30, time.Hour)
			if total, unacked := s.Dropped(); total != tt.total || unacked != tt.unacked {
				t.Errorf("total=%d unacked=%d, want total=%d unacked=%d",
					total, unacked, tt.total, tt.unacked)
			}
		})
	}
}

func TestOpenPersistsRepairedAckProgress(t *testing.T) {
	// Successive crashes: an ack-crash leaves acked stale-low on disk
	// ("100 95" with unacked 0). If Open only repaired in memory, the
	// five units of slack would let a later drop-crash (unacked written,
	// state write lost) hide the drop: max(100, 95+1) = 100. Open must
	// persist the repaired state so the slack is gone.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, droppedFile), []byte("0"), 0o600)
	os.WriteFile(filepath.Join(dir, droppedStateFile), []byte("100 95"), 0o600)
	s := mustOpen(t, dir, 1<<30, time.Hour)
	s.Close()
	if b, err := os.ReadFile(filepath.Join(dir, droppedStateFile)); err != nil || string(b) != "100 100" {
		t.Fatalf("repaired state not persisted: %q, %v, want \"100 100\"", b, err)
	}

	// The later drop-crash: only the unacked sidecar was written.
	os.WriteFile(filepath.Join(dir, droppedFile), []byte("1"), 0o600)
	s2 := mustOpen(t, dir, 1<<30, time.Hour)
	if total, unacked := s2.Dropped(); total != 101 || unacked != 1 {
		t.Errorf("after drop-crash: total=%d unacked=%d, want total=101 unacked=1", total, unacked)
	}
}

func TestOpenFailsOnUnreadableSidecar(t *testing.T) {
	// An OPERATIONAL read error on an existing sidecar (permissions, I/O)
	// must fail Open and leave the file alone: reconstructing over contents
	// we could not see would erase them — e.g. rollback-era drops behind an
	// all-acked state.
	if os.Geteuid() == 0 {
		t.Skip("file modes do not bind root")
	}
	for _, name := range []string{droppedFile, droppedStateFile} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, droppedFile), []byte("5"), 0o600)
			os.WriteFile(filepath.Join(dir, droppedStateFile), []byte("100 100"), 0o600)
			if err := os.Chmod(filepath.Join(dir, name), 0o000); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(filepath.Join(dir, name), 0o600) })
			if _, err := Open(dir, 1<<30, time.Hour); err == nil {
				t.Fatal("Open must fail on an unreadable sidecar")
			}
			os.Chmod(filepath.Join(dir, name), 0o600)
			if b, err := os.ReadFile(filepath.Join(dir, droppedFile)); err != nil || string(b) != "5" {
				t.Errorf("unacked sidecar was altered: %q, %v, want \"5\"", b, err)
			}
		})
	}
}

func TestOpenReconstructsMissingUnacked(t *testing.T) {
	// A lost legacy sidecar must not silently convert the outstanding
	// remainder into acknowledged state on disk: the reconstruction is
	// persisted, recreating the sidecar with total-acked.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, droppedStateFile), []byte("100 95"), 0o600)
	s := mustOpen(t, dir, 1<<30, time.Hour)
	if total, unacked := s.Dropped(); total != 100 || unacked != 5 {
		t.Fatalf("total=%d unacked=%d, want total=100 unacked=5", total, unacked)
	}
	s.Close()
	if b, err := os.ReadFile(filepath.Join(dir, droppedFile)); err != nil || string(b) != "5" {
		t.Errorf("unacked sidecar not recreated: %q, %v, want \"5\"", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, droppedStateFile)); err != nil || string(b) != "100 95" {
		t.Errorf("state sidecar changed: %q, %v, want \"100 95\"", b, err)
	}
}

func TestMaxAgeDropsOldSegments(t *testing.T) {
	dir := t.TempDir()
	s := mustOpen(t, dir, 1<<30, time.Hour)
	appendN(t, s, 0, 2)
	s.Close()
	// Backdate the segment past max_age.
	segs, _ := s.listSegments()
	old := time.Now().Add(-2 * time.Hour)
	os.Chtimes(segs[0].path, old, old)

	s2 := mustOpen(t, dir, 1<<30, time.Hour) // bounds are enforced at Open
	appendN(t, s2, 2, 1)
	if total, _ := s2.Dropped(); total != 2 {
		t.Errorf("dropped = %d, want 2 (aged-out segment)", total)
	}
	got, _ := s2.Next(100)
	if len(got) != 1 || got[0].ProbeId != "probe-000002" {
		t.Errorf("surviving records wrong: %d", len(got))
	}
}

func TestAppendOversizedRecordDroppedNotFatal(t *testing.T) {
	dir := t.TempDir()
	s := mustOpen(t, dir, 1<<30, time.Hour)
	big := result(0)
	big.Error = string(make([]byte, maxRecordBytes))
	if err := s.Append(big); err != nil {
		t.Fatalf("oversized record must be dropped, not returned as an error: %v", err)
	}
	if total, _ := s.Dropped(); total != 1 {
		t.Errorf("dropped = %d, want 1", total)
	}
	if s.Pending() != 0 {
		t.Errorf("pending = %d, want 0", s.Pending())
	}
	// The spool keeps working, and the drop survives a restart.
	appendN(t, s, 1, 1)
	s.Close()
	s2 := mustOpen(t, dir, 1<<30, time.Hour)
	if total, _ := s2.Dropped(); total != 1 {
		t.Errorf("dropped counter lost across reopen: %d", total)
	}
	if got, _ := s2.Next(100); len(got) != 1 || got[0].ProbeId != "probe-000001" {
		t.Errorf("surviving record wrong: %d", len(got))
	}
}

func TestAppendIOErrorIsReturned(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory modes do not bind root")
	}
	dir := t.TempDir()
	s := mustOpen(t, dir, 1<<30, time.Hour)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	if err := s.Append(result(0)); err == nil {
		t.Fatal("append into an unwritable spool dir must return an error (fatal for the agent)")
	}
	// The per-record drop path must not mask spool I/O either: an oversized
	// record whose drop counter cannot be persisted is a fatal error too.
	big := result(1)
	big.Error = string(make([]byte, maxRecordBytes))
	if err := s.Append(big); err == nil {
		t.Fatal("unpersistable drop counter must return an error (fatal for the agent)")
	}
}

func TestOpenPrunesExpiredSegments(t *testing.T) {
	dir := t.TempDir()
	s := mustOpen(t, dir, 1<<30, time.Hour)
	appendN(t, s, 0, 3)
	s.Close()
	segs, _ := s.listSegments()
	old := time.Now().Add(-2 * time.Hour)
	for _, seg := range segs {
		os.Chtimes(seg.path, old, old)
	}
	// Startup drops must add to a prior persisted total, not overwrite it.
	os.WriteFile(filepath.Join(dir, droppedFile), []byte("5"), 0o600)

	s2 := mustOpen(t, dir, 1<<30, time.Hour)
	if s2.Pending() != 0 {
		t.Errorf("pending = %d, want 0 (expired segments pruned at Open)", s2.Pending())
	}
	if total, _ := s2.Dropped(); total != 8 {
		t.Errorf("dropped = %d, want 8 (5 prior + 3 pruned)", total)
	}
	if segs, _ := s2.listSegments(); len(segs) != 0 {
		t.Errorf("%d segments survive Open, want 0", len(segs))
	}
	select {
	case <-s2.C():
		t.Error("wake emitted although pruning removed every pending result")
	default:
	}
}

func TestOpenPrunesOversizedRecovery(t *testing.T) {
	dir := t.TempDir()
	// One ~2 KiB segment per open/append/close cycle (reopen starts a fresh
	// sequence number, so appends never land in a sealed segment).
	for cycle := range 4 {
		s, err := Open(dir, 1<<30, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		r := result(cycle)
		r.Error = string(make([]byte, 2048))
		if err := s.Append(r); err != nil {
			t.Fatal(err)
		}
		s.Close()
	}

	s2 := mustOpen(t, dir, 5000, time.Hour)
	segs, _ := s2.listSegments()
	var total int64
	for _, seg := range segs {
		total += seg.size
	}
	if total > 5000 {
		t.Errorf("recovered spool is %d bytes, exceeds max_bytes 5000", total)
	}
	if total, _ := s2.Dropped(); total != 2 {
		t.Errorf("dropped = %d, want 2 (oldest segments pruned at Open)", total)
	}
	got, _ := s2.Next(100)
	if len(got) != 2 || got[0].ProbeId != "probe-000002" {
		t.Errorf("surviving records wrong: len=%d", len(got))
	}
	select {
	case <-s2.C():
	default:
		t.Error("surviving recovered results must signal the pusher")
	}
}

func TestNotifyWakes(t *testing.T) {
	s := mustOpen(t, t.TempDir(), 1<<30, time.Hour)
	select {
	case <-s.C():
		t.Fatal("spurious wake on empty spool")
	default:
	}
	appendN(t, s, 0, 1)
	select {
	case <-s.C():
	case <-time.After(time.Second):
		t.Fatal("append did not signal")
	}
}
