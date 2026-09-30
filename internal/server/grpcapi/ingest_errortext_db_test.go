package grpcapi

// DB-backed test for #227: error text Postgres cannot store must never
// poison a result batch. Gated on POLARBEAM_TEST_DB_URL (see
// internal/server/dbtest).
//
// The unit table in ingest_test.go pins the sanitized string; this pins the
// consequence that made it a P1 — one unstorable row failed the whole
// INSERT, PushResults answered Unavailable, and the agent retried the same
// head-of-spool batch forever, so nothing behind it ever drained.

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
)

func TestPushResultsUnstorableErrorTextDoesNotStallIngest(t *testing.T) {
	t.Parallel()
	ctx, s, raw := streamHarness(t)
	agentID, serial := streamSetup(t, ctx, s)
	srv := New(s, nil)

	assigned, err := srv.agentProbeMap(ctx, agentID)
	if err != nil {
		t.Fatalf("agentProbeMap: %v", err)
	}
	var probeID, targetID uuid.UUID
	for p, a := range assigned {
		probeID, targetID = p, a.TargetID
		break
	}
	if probeID == uuid.Nil {
		t.Fatal("no probe assignments — the mesh did not expand")
	}

	base := time.Now().Add(-time.Minute).Truncate(time.Second)
	result := func(offset time.Duration, st pb.ProbeStatus, errText string) *pb.ProbeResult {
		r := &pb.ProbeResult{
			ProbeId:   probeID.String(),
			TargetId:  targetID.String(),
			Type:      pb.ProbeType_PROBE_TYPE_ICMP,
			StartedAt: timestamppb.New(base.Add(offset)),
			Status:    st,
			Sent:      1,
			JitterUs:  -1,
			Error:     errText,
		}
		if st == pb.ProbeStatus_PROBE_STATUS_OK {
			r.Received = 1
		}
		return r
	}
	pctx := agentPeerCtx(ctx, agentID, serial)

	// A rune straddling the 128-byte cut (the issue's repro) and a NUL
	// smuggled in through a TLS server's SAN, beside a healthy result.
	batch1 := []*pb.ProbeResult{
		result(0, pb.ProbeStatus_PROBE_STATUS_DNS_FAILURE,
			"lookup "+strings.Repeat("界", 50)+".invalid: no such host"),
		result(time.Second, pb.ProbeStatus_PROBE_STATUS_TLS_FAILURE,
			"x509: certificate is valid for evil\x00.example, not target.example"),
		result(2*time.Second, pb.ProbeStatus_PROBE_STATUS_OK, ""),
	}
	resp, err := srv.PushResults(pctx, &pb.PushResultsRequest{Results: batch1})
	if err != nil {
		t.Fatalf("batch with unstorable error text: PushResults = %v; the agent would retry it forever", err)
	}
	if resp.GetAccepted() != 3 {
		t.Fatalf("batch 1 accepted %d, want 3", resp.GetAccepted())
	}

	// The next batch drains normally.
	resp, err = srv.PushResults(pctx, &pb.PushResultsRequest{Results: []*pb.ProbeResult{
		result(3*time.Second, pb.ProbeStatus_PROBE_STATUS_OK, ""),
	}})
	if err != nil {
		t.Fatalf("follow-up batch: PushResults = %v", err)
	}
	if resp.GetAccepted() != 1 {
		t.Fatalf("batch 2 accepted %d, want 1", resp.GetAccepted())
	}

	rows, err := raw.Query(ctx,
		`SELECT error FROM probe_results WHERE agent_id = $1 AND error IS NOT NULL ORDER BY time`, agentID)
	if err != nil {
		t.Fatalf("read back errors: %v", err)
	}
	defer rows.Close()
	var stored []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			t.Fatalf("scan error: %v", err)
		}
		stored = append(stored, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read back errors: %v", err)
	}
	if len(stored) != 2 {
		t.Fatalf("stored %d error rows, want 2: %q", len(stored), stored)
	}
	for _, e := range stored {
		if !utf8.ValidString(e) || strings.ContainsRune(e, 0) || len(e) > maxErrorLen {
			t.Errorf("stored error %q is not storable text within %d bytes", e, maxErrorLen)
		}
	}
}
