package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ResultRow is one probe_results row ready for insertion. Nil pointers map
// to SQL NULL ("not measured" / no error).
type ResultRow struct {
	Time      time.Time
	TargetID  uuid.UUID
	ProbeID   uuid.UUID
	ProbeType int16
	Status    int16
	Sent      int32
	Received  int32
	LossPct   *float32

	RttMinUS    *int32
	RttAvgUS    *int32
	RttMaxUS    *int32
	RttStddevUS *int32
	JitterUS    *int32

	DNSUS          *int32
	TCPConnectUS   *int32
	TLSHandshakeUS *int32
	TTFBUS         *int32
	TotalUS        *int32

	Error *string

	// Traceroute rides the row through the inserted-filter but is not a
	// hypertable column: hops go to traceroute_current/path_events via
	// pathwatch, and only for rows the insert genuinely added.
	Traceroute *TraceroutePayload
}

// TraceroutePayload is the wire TracerouteResult mapped for pathwatch.
type TraceroutePayload struct {
	DestReached bool
	PathHash    []byte
	Hops        []byte // JSON array mirroring the wire Hop messages
}

// InsertResultsTx bulk-inserts a batch for one agent in a single statement
// on the caller's transaction. The agent ID comes from the caller's
// authenticated mTLS identity, never from the batch. Duplicates
// (at-least-once spool replay) are silently dropped by the dedupe index;
// the returned slice holds only the rows the insert genuinely added, so
// downstream bookkeeping (outage hysteresis, pathwatch) can never
// double-count a replayed result.
func InsertResultsTx(ctx context.Context, tx pgx.Tx, agentID uuid.UUID, rows []ResultRow) ([]ResultRow, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	n := len(rows)
	var (
		times                                  = make([]time.Time, n)
		targetIDs, probeIDs                    = make([]uuid.UUID, n), make([]uuid.UUID, n)
		probeTypes, statuses                   = make([]int16, n), make([]int16, n)
		sents, receiveds                       = make([]int32, n), make([]int32, n)
		lossPcts                               = make([]*float32, n)
		rttMins, rttAvgs, rttMaxs, rttStddevs  = make([]*int32, n), make([]*int32, n), make([]*int32, n), make([]*int32, n)
		jitters, dnss, tcps, tlss, ttfbs, tots = make([]*int32, n), make([]*int32, n), make([]*int32, n), make([]*int32, n), make([]*int32, n), make([]*int32, n)
		errs                                   = make([]*string, n)
	)
	for i, r := range rows {
		times[i] = r.Time
		targetIDs[i], probeIDs[i] = r.TargetID, r.ProbeID
		probeTypes[i], statuses[i] = r.ProbeType, r.Status
		sents[i], receiveds[i] = r.Sent, r.Received
		lossPcts[i] = r.LossPct
		rttMins[i], rttAvgs[i], rttMaxs[i], rttStddevs[i] = r.RttMinUS, r.RttAvgUS, r.RttMaxUS, r.RttStddevUS
		jitters[i], dnss[i], tcps[i], tlss[i], ttfbs[i], tots[i] = r.JitterUS, r.DNSUS, r.TCPConnectUS, r.TLSHandshakeUS, r.TTFBUS, r.TotalUS
		errs[i] = r.Error
	}

	ret, err := tx.Query(ctx, `
		INSERT INTO probe_results (time, agent_id, target_id, probe_id, probe_type, status,
			sent, received, loss_pct, rtt_min_us, rtt_avg_us, rtt_max_us, rtt_stddev_us,
			jitter_us, dns_us, tcp_connect_us, tls_handshake_us, ttfb_us, total_us, error)
		SELECT u.time, $1, u.target_id, u.probe_id, u.probe_type, u.status,
			u.sent, u.received, u.loss_pct, u.rtt_min_us, u.rtt_avg_us, u.rtt_max_us, u.rtt_stddev_us,
			u.jitter_us, u.dns_us, u.tcp_connect_us, u.tls_handshake_us, u.ttfb_us, u.total_us, u.error
		FROM unnest($2::timestamptz[], $3::uuid[], $4::uuid[], $5::smallint[], $6::smallint[],
			$7::int[], $8::int[], $9::real[], $10::int[], $11::int[], $12::int[], $13::int[],
			$14::int[], $15::int[], $16::int[], $17::int[], $18::int[], $19::int[], $20::text[])
			AS u(time, target_id, probe_id, probe_type, status, sent, received, loss_pct,
				rtt_min_us, rtt_avg_us, rtt_max_us, rtt_stddev_us, jitter_us,
				dns_us, tcp_connect_us, tls_handshake_us, ttfb_us, total_us, error)
		ON CONFLICT DO NOTHING
		RETURNING probe_id, time`,
		agentID, times, targetIDs, probeIDs, probeTypes, statuses, sents, receiveds, lossPcts,
		rttMins, rttAvgs, rttMaxs, rttStddevs, jitters, dnss, tcps, tlss, ttfbs, tots, errs)
	if err != nil {
		return nil, fmt.Errorf("insert results: %w", err)
	}
	defer ret.Close()

	// Map the RETURNING set back to input rows. Postgres stores timestamptz
	// at microsecond precision, so input times are keyed truncated.
	insertedKeys := make(map[resultKey]struct{}, len(rows))
	for ret.Next() {
		var probeID uuid.UUID
		var t time.Time
		if err := ret.Scan(&probeID, &t); err != nil {
			return nil, fmt.Errorf("scan inserted result: %w", err)
		}
		insertedKeys[resultKey{probeID, t.UTC().UnixMicro()}] = struct{}{}
	}
	if err := ret.Err(); err != nil {
		return nil, fmt.Errorf("insert results: %w", err)
	}

	inserted := make([]ResultRow, 0, len(insertedKeys))
	for _, r := range rows {
		k := resultKey{r.ProbeID, r.Time.UTC().UnixMicro()}
		if _, ok := insertedKeys[k]; ok {
			inserted = append(inserted, r)
			delete(insertedKeys, k) // in-batch duplicates count once
		}
	}
	return inserted, nil
}

type resultKey struct {
	probeID uuid.UUID
	timeUS  int64
}

// Ingest assignment checks moved to grpcapi's assignmentCache, which
// derives each agent's probe→target map from the same meshexpand snapshot
// the agent receives — a (probe, target) pair check rather than the old
// target-only TargetAssignedToAgent query, so results for deleted or
// disabled probes are rejected even while their target stays assigned
// through another probe config.
