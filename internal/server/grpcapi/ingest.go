// PushResults ingestion: wire → row mapping and probe-assignment checks.
package grpcapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
	"github.com/devalexllc/polarbeam/internal/server/meshexpand"
	"github.com/devalexllc/polarbeam/internal/server/mtuwatch"
	"github.com/devalexllc/polarbeam/internal/server/outage"
	"github.com/devalexllc/polarbeam/internal/server/pathwatch"
	"github.com/devalexllc/polarbeam/internal/server/store"
	"github.com/devalexllc/polarbeam/internal/server/thresholds"
)

const (
	// Sanity cap well above the agent's ≤500 batch size; anything larger is
	// a broken or hostile client.
	maxBatchSize = 5000
	// Results stamped further in the future than this are clock garbage.
	maxFutureSkew = 5 * time.Minute
	// error text is truncated to keep hypertable rows narrow.
	maxErrorLen = 128

	assignmentCacheTTL = 30 * time.Second
)

// assignmentCache memoizes each agent's expanded probe assignments so a
// batch costs at most one config load. Both presence and absence within the
// TTL are trusted — assignment changes converge within 30 s, same as config
// distribution. Checking the (probe, target) PAIR — not just the target —
// matters for cleanup durability: a spooled result for a deleted or
// disabled probe would otherwise slip through whenever its target is still
// assigned via another probe config, recreating retired series_state and
// reopening an incident that nothing will ever close.
//
// Each assignment also carries the direction's effective critical
// thresholds, resolved at rebuild time, so degraded grading costs nothing
// per result. Threshold edits therefore converge within the same 30 s as
// assignments, and spool-replayed history is graded at replay-time values —
// both accepted (see the outage package doc).
type assignmentCache struct {
	mu      sync.Mutex
	entries map[uuid.UUID]assignmentEntry
}

// probeAssignment is one probe the agent is expected to run: its configured
// target plus the effective critical thresholds for the direction.
type probeAssignment struct {
	TargetID uuid.UUID
	Crit     thresholds.T
}

type assignmentEntry struct {
	probes  map[uuid.UUID]probeAssignment
	expires time.Time
}

func (c *assignmentCache) lookup(agentID uuid.UUID, now time.Time) (map[uuid.UUID]probeAssignment, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, found := c.entries[agentID]
	if !found || now.After(e.expires) {
		return nil, false
	}
	return e.probes, true
}

func (c *assignmentCache) put(agentID uuid.UUID, probes map[uuid.UUID]probeAssignment, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[uuid.UUID]assignmentEntry)
	}
	// Opportunistic sweep keeps the map bounded without a background task.
	if len(c.entries) > 4096 {
		for k, e := range c.entries {
			if now.After(e.expires) {
				delete(c.entries, k)
			}
		}
	}
	c.entries[agentID] = assignmentEntry{probes: probes, expires: now.Add(assignmentCacheTTL)}
}

// agentProbeMap returns the agent's current probe assignments, derived from
// the SAME expansion that builds config snapshots (meshexpand.BuildSnapshot),
// so ingest can never accept a probe ID the agent wasn't told to run. Each
// assignment carries the direction's effective critical thresholds: the
// global dashboard_settings merged with the (src site, dst site) override;
// external targets have no dst site and grade on the global values.
func (s *Server) agentProbeMap(ctx context.Context, agentID uuid.UUID) (map[uuid.UUID]probeAssignment, error) {
	now := time.Now()
	if m, hit := s.assignments.lookup(agentID, now); hit {
		return m, nil
	}
	in, err := s.store.LoadAgentConfigInputs(ctx, agentID)
	if err != nil {
		return nil, err
	}
	snap, err := meshexpand.BuildSnapshot(in)
	if err != nil {
		return nil, err
	}
	settings, err := s.store.GetSettings(ctx)
	if err != nil {
		return nil, err
	}
	overrides, err := s.store.PathThresholdPairs(ctx, in.SiteID)
	if err != nil {
		return nil, err
	}
	global := thresholds.T{
		LatencyWarnUS: settings.LatencyWarnUS,
		LatencyCritUS: settings.LatencyCritUS,
		LossWarnPct:   settings.LossWarnPct,
		LossCritPct:   settings.LossCritPct,
	}
	overrideByPair := make(map[sitePair]thresholds.Override, len(overrides))
	for _, o := range overrides {
		// Rows are already stored in canonical (bytewise) order.
		overrideByPair[sitePair{o.SiteAID, o.SiteBID}] = thresholds.Override{
			LatencyWarnUS: o.LatencyWarnUS,
			LatencyCritUS: o.LatencyCritUS,
			LossWarnPct:   o.LossWarnPct,
			LossCritPct:   o.LossCritPct,
		}
	}
	targetSite := make(map[uuid.UUID]uuid.UUID, len(in.Peers))
	for _, p := range in.Peers {
		targetSite[p.TargetID] = p.SiteID
	}
	for _, d := range in.Direct {
		if d.DstSiteID != nil {
			targetSite[d.TargetID] = *d.DstSiteID
		}
	}
	specs := snap.GetProbes()
	m := make(map[uuid.UUID]probeAssignment, len(specs))
	for _, spec := range specs {
		probeID, err := uuid.Parse(spec.GetProbeId())
		if err != nil {
			continue // cannot happen: snapshot IDs are server-derived
		}
		targetID, err := uuid.Parse(spec.GetTarget().GetTargetId())
		if err != nil {
			continue
		}
		crit := global
		if dstSite, hasSite := targetSite[targetID]; hasSite {
			if o, hasOverride := overrideByPair[canonicalPair(in.SiteID, dstSite)]; hasOverride {
				crit = thresholds.Effective(global, o)
			}
		}
		m[probeID] = probeAssignment{TargetID: targetID, Crit: crit}
	}
	s.assignments.put(agentID, m, now)
	return m, nil
}

// sitePair is an unordered site pair in the canonical (uuid bytewise) order
// path_thresholds stores.
type sitePair struct{ a, b uuid.UUID }

func canonicalPair(a, b uuid.UUID) sitePair {
	if bytes.Compare(a[:], b[:]) > 0 {
		a, b = b, a
	}
	return sitePair{a, b}
}

// resultToRow maps one wire ProbeResult to a hypertable row. Pure: all
// validation failures return an error naming the problem. now is injected
// for testability.
func resultToRow(r *pb.ProbeResult, now time.Time) (store.ResultRow, error) {
	var row store.ResultRow

	probeID, err := uuid.Parse(r.GetProbeId())
	if err != nil {
		return row, fmt.Errorf("bad probe_id %q", r.GetProbeId())
	}
	targetID, err := uuid.Parse(r.GetTargetId())
	if err != nil {
		return row, fmt.Errorf("bad target_id %q", r.GetTargetId())
	}
	if r.GetStartedAt() == nil {
		return row, fmt.Errorf("missing started_at")
	}
	t := r.GetStartedAt().AsTime()
	if t.After(now.Add(maxFutureSkew)) {
		return row, fmt.Errorf("started_at %s is in the future", t.Format(time.RFC3339))
	}
	if r.GetStatus() == pb.ProbeStatus_PROBE_STATUS_UNSPECIFIED {
		return row, fmt.Errorf("status unspecified")
	}

	row = store.ResultRow{
		Time:      t,
		TargetID:  targetID,
		ProbeID:   probeID,
		ProbeType: int16(r.GetType()),
		Status:    int16(r.GetStatus()),
		Sent:      int32(min(r.GetSent(), 1<<30)),
		Received:  int32(min(r.GetReceived(), 1<<30)),
	}
	if row.Sent > 0 {
		loss := float32(row.Sent-row.Received) / float32(row.Sent) * 100
		row.LossPct = &loss
	}
	if rtt := r.GetRtt(); rtt != nil {
		row.RttMinUS = usColumn(rtt.GetMinUs())
		row.RttAvgUS = usColumn(rtt.GetAvgUs())
		row.RttMaxUS = usColumn(rtt.GetMaxUs())
		row.RttStddevUS = usColumn(rtt.GetStddevUs())
	}
	row.JitterUS = usColumn(r.GetJitterUs())
	if tm := r.GetTimings(); tm != nil {
		row.DNSUS = usColumn(tm.GetDnsUs())
		row.TCPConnectUS = usColumn(tm.GetTcpConnectUs())
		row.TLSHandshakeUS = usColumn(tm.GetTlsHandshakeUs())
		row.TTFBUS = usColumn(tm.GetTtfbUs())
		row.TotalUS = usColumn(tm.GetTotalUs())
	}
	if e := r.GetError(); e != "" {
		if len(e) > maxErrorLen {
			e = e[:maxErrorLen]
		}
		row.Error = &e
	}
	if tr := r.GetTraceroute(); tr != nil {
		payload, err := tracePayload(tr)
		if err != nil {
			return row, err
		}
		row.Traceroute = payload
	}
	if m := r.GetPathMtu(); m != nil {
		payload, err := pathMtuPayload(m, r.GetStatus())
		if err != nil {
			return row, err
		}
		row.PathMtu = payload
	}
	return row, nil
}

// maxTracerouteHops caps hop counts from the wire; the prober sends at most
// 30, anything beyond 64 is a broken or hostile client.
const maxTracerouteHops = 64

// tracePayload maps the wire TracerouteResult to the pathwatch payload,
// marshaling hops to the JSON shape stored in traceroute_current/path_events.
func tracePayload(tr *pb.TracerouteResult) (*store.TraceroutePayload, error) {
	if len(tr.GetHops()) > maxTracerouteHops {
		return nil, fmt.Errorf("traceroute with %d hops exceeds the %d-hop limit", len(tr.GetHops()), maxTracerouteHops)
	}
	if tr.GetDestReached() && len(tr.GetPathHash()) != sha256.Size {
		return nil, fmt.Errorf("traceroute path_hash is %d bytes, want %d", len(tr.GetPathHash()), sha256.Size)
	}
	type hopJSON struct {
		TTL   uint32   `json:"ttl"`
		Addrs []string `json:"addrs"`
		RTTUS []int64  `json:"rtt_us"`
	}
	hops := make([]hopJSON, len(tr.GetHops()))
	for i, h := range tr.GetHops() {
		addrs := h.GetAddrs()
		if addrs == nil {
			addrs = []string{}
		}
		rtts := h.GetRttUs()
		if rtts == nil {
			rtts = []int64{}
		}
		hops[i] = hopJSON{TTL: h.GetTtl(), Addrs: addrs, RTTUS: rtts}
	}
	raw, err := json.Marshal(hops)
	if err != nil {
		return nil, fmt.Errorf("marshal traceroute hops: %w", err)
	}
	return &store.TraceroutePayload{
		DestReached: tr.GetDestReached(),
		PathHash:    tr.GetPathHash(),
		Hops:        raw,
	}, nil
}

// maxPathMtuBytes caps sizes from the wire; the prober tests at most 9216
// bytes, anything beyond 64 KiB is a broken or hostile client.
const maxPathMtuBytes = 1 << 16

// pathMtuPayload maps the wire PathMtuResult to the mtuwatch payload. A
// measurement is usable when it converged on a delivered size: OK runs,
// and black-hole runs (which report TIMEOUT but bracket the MTU just as
// tightly). Non-converged partials would flap current/events.
func pathMtuPayload(m *pb.PathMtuResult, st pb.ProbeStatus) (*store.PathMtuPayload, error) {
	if m.GetLargestOkBytes() > maxPathMtuBytes || m.GetSmallestFailedBytes() > maxPathMtuBytes ||
		m.GetNextHopMtuBytes() > maxPathMtuBytes {
		return nil, fmt.Errorf("path MTU sizes exceed the %d-byte limit", maxPathMtuBytes)
	}
	if v := m.GetIpVersion(); v != 4 && v != 6 {
		return nil, fmt.Errorf("path MTU ip_version %d, want 4 or 6", v)
	}
	usable := m.GetLargestOkBytes() > 0 &&
		(st == pb.ProbeStatus_PROBE_STATUS_OK ||
			(st == pb.ProbeStatus_PROBE_STATUS_TIMEOUT && m.GetBlackHoleSuspected()))
	return &store.PathMtuPayload{
		LargestOK:       int32(m.GetLargestOkBytes()),
		SmallestFailed:  int32(m.GetSmallestFailedBytes()),
		NextHopMTU:      int32(m.GetNextHopMtuBytes()),
		IPVersion:       int16(m.GetIpVersion()),
		BlackHole:       m.GetBlackHoleSuspected(),
		LocalConstraint: m.GetLocalConstraint(),
		RttUS:           usColumn(m.GetRttUs()),
		Usable:          usable,
	}, nil
}

// toMTURuns extracts the path MTU payloads from genuinely inserted rows.
func toMTURuns(rows []store.ResultRow) []mtuwatch.Run {
	var runs []mtuwatch.Run
	for _, r := range rows {
		if r.PathMtu == nil {
			continue
		}
		runs = append(runs, mtuwatch.Run{
			ProbeID:         r.ProbeID,
			TargetID:        r.TargetID,
			Time:            r.Time,
			LargestOK:       r.PathMtu.LargestOK,
			SmallestFailed:  r.PathMtu.SmallestFailed,
			NextHopMTU:      r.PathMtu.NextHopMTU,
			IPVersion:       r.PathMtu.IPVersion,
			BlackHole:       r.PathMtu.BlackHole,
			LocalConstraint: r.PathMtu.LocalConstraint,
			RttUS:           r.PathMtu.RttUS,
			Usable:          r.PathMtu.Usable,
		})
	}
	return runs
}

// toPathRuns extracts the traceroute payloads from genuinely inserted rows.
func toPathRuns(rows []store.ResultRow) []pathwatch.Run {
	var runs []pathwatch.Run
	for _, r := range rows {
		if r.Traceroute == nil {
			continue
		}
		runs = append(runs, pathwatch.Run{
			ProbeID:     r.ProbeID,
			TargetID:    r.TargetID,
			Time:        r.Time,
			DestReached: r.Traceroute.DestReached,
			PathHash:    r.Traceroute.PathHash,
			Hops:        r.Traceroute.Hops,
		})
	}
	return runs
}

// toOutageResults maps genuinely inserted rows to the outage package's
// input. UNSUPPORTED and every other non-OK status count as failures. OK
// rows are graded against their assignment's effective critical thresholds;
// failures are never graded (their metrics describe the failure, not the
// link).
func toOutageResults(rows []store.ResultRow, assigned map[uuid.UUID]probeAssignment) []outage.Result {
	out := make([]outage.Result, len(rows))
	for i, r := range rows {
		var errText string
		if r.Error != nil {
			errText = *r.Error
		}
		res := outage.Result{
			ProbeID:    r.ProbeID,
			TargetID:   r.TargetID,
			ProbeType:  r.ProbeType,
			Time:       r.Time,
			OK:         r.Status == int16(pb.ProbeStatus_PROBE_STATUS_OK),
			StatusCode: r.Status,
			Error:      errText,
		}
		if res.OK {
			if a, ok := assigned[r.ProbeID]; ok {
				var loss *float64
				if r.LossPct != nil {
					v := float64(*r.LossPct)
					loss = &v
				}
				res.Degraded, res.DegradedDetail = thresholds.GradeCrit(a.Crit, rowLatencyUS(r), loss)
			}
		}
		out[i] = res
	}
	return out
}

// rowLatencyUS collapses a row's timing families to one headline latency:
// the first measured value in purity order, mirroring the read side's
// latencyExpr COALESCE ladder (store/dashboard.go) so grading and display
// judge the same number.
func rowLatencyUS(r store.ResultRow) *int64 {
	for _, v := range []*int32{r.RttAvgUS, r.TCPConnectUS, r.TLSHandshakeUS, r.TTFBUS, r.TotalUS} {
		if v != nil {
			us := int64(*v)
			return &us
		}
	}
	return nil
}

// usColumn converts a wire microsecond value to a nullable column: negative
// means "not measured" (NULL), and values beyond int32 are clamped (an int32
// of microseconds is ~35 minutes — far past any probe timeout).
func usColumn(us int64) *int32 {
	if us < 0 {
		return nil
	}
	v := int32(min(us, 1<<31-1))
	return &v
}
