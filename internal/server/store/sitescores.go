package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// SiteScore is one site's month-to-date sample tallies for the map card.
// OK/Samples is the availability score; HealthyOK/OK is the performance
// score. Ratios are left to the caller so a zero denominator stays visibly
// "no data" instead of an invented 100 %.
type SiteScore struct {
	Name      string
	Samples   int64
	OK        int64
	HealthyOK int64
}

// SiteScores tallies, for every visible site, the probe samples since `since`
// across each probe series the site's agents RUN and each series that
// TARGETS one of the site's agents (a series whose both ends sit at one site
// counts once). Reads the probe_results_hourly cagg (migration 0002; 100 d
// retention, so a calendar month is always covered); materialized_only =
// false serves the current hour live from raw.
//
// The grading unit is one (agent, target, probe_type) series per hourly
// bucket — the cagg carries no probe_id, and the matrix and pair views
// aggregate at the same grain, so two same-type templates between the same
// endpoints grade as one blended series (see AddMeshProbe). The cagg's
// latency_source partitions are folded back together first: a series'
// timing family is stable in practice (ICMP → rtt, TCP → tcp_connect), and
// loss must be summed across them regardless.
//
// A bucket is healthy when its average latency and loss both stay below the
// WARN thresholds effective for that direction — the same four-layer merge
// ingest applies for the crit tier (thresholds.Effective: pair on the
// source agent's plane, pair all-planes, plane default, global), expressed
// as COALESCE per field, and the same comparison as thresholds.GradeWarn
// (unmeasured never breaches; zero loss never breaches). The parity DB test
// replays thresholds/testdata/threshold-merge.json through both.
// least/greatest on uuid is the bytewise canonical order path_thresholds
// stores. Loss folds failed train samples too (a timed-out train IS loss),
// so an hour with failures can grade unhealthy on loss as well as counting
// against availability. Loss divides the LOST count (sent - received), not
// 1 - received/sent: 1 - 93/100 is 0.06999… in binary floating point and
// would slip an hour sitting exactly on a 7 % warn under the threshold.
//
// Rows of excludeProbeType are skipped: the caller passes traceroute, whose
// run-accounting rows would poison the ratio exactly as in AgentHealthSeries
// (store deliberately does not import pb). External and deleted targets
// have no destination site: they score for the source site only and, like
// ingest, skip both pair layers. A series whose source agent row is gone is
// dropped — its plane and site are unknowable, the same fail-closed rule
// ListOutages applies. networks is the caller's network scope (nil =
// unfiltered): series are filtered by the source agent's plane BEFORE the
// month is grouped (as AgentHealthSeries filters its cagg read), so a
// tenant's poll never folds other planes' history, and sites by
// siteScopePredicate, so a scoped caller sees only its planes' samples on a
// shared site. Every visible site yields a row, zeros when it has no
// samples. Ordered by site name.
func (s *Store) SiteScores(ctx context.Context, since time.Time, excludeProbeType int16, networks []uuid.UUID) ([]SiteScore, error) {
	rows, err := s.pool.Query(ctx, `
		WITH series_buckets AS (
			SELECT h.bucket, h.agent_id, h.target_id, h.probe_type,
			       sum(h.samples)::bigint    AS samples,
			       sum(h.ok_samples)::bigint AS ok_samples,
			       sum(h.lat_sum_us)::float8 / NULLIF(sum(h.lat_count), 0)::float8 AS lat_avg_us,
			       100.0 * (sum(h.sent) - sum(h.received))::float8 / NULLIF(sum(h.sent), 0)::float8 AS loss_pct
			  FROM probe_results_hourly h
			 WHERE h.bucket >= $1::timestamptz
			   AND h.probe_type <> $2
			   AND ($3::uuid[] IS NULL
			        OR h.agent_id IN (SELECT id FROM agents WHERE network_id = ANY($3)))
			 GROUP BY h.bucket, h.agent_id, h.target_id, h.probe_type
		),
		graded AS (
			SELECT sb.samples, sb.ok_samples,
			       sa.site_id AS src_site_id,
			       da.site_id AS dst_site_id,
			       (sb.lat_avg_us IS NULL
			         OR sb.lat_avg_us < COALESCE(pn.latency_warn_us, pa.latency_warn_us, nt.latency_warn_us, ds.latency_warn_us))
			       AND (sb.loss_pct IS NULL OR sb.loss_pct <= 0
			         OR sb.loss_pct < COALESCE(pn.loss_warn_pct, pa.loss_warn_pct, nt.loss_warn_pct, ds.loss_warn_pct)) AS healthy
			  FROM series_buckets sb
			  JOIN agents sa ON sa.id = sb.agent_id
			  LEFT JOIN targets t ON t.id = sb.target_id
			  LEFT JOIN agents da ON da.id = t.agent_id
			  CROSS JOIN dashboard_settings ds
			  LEFT JOIN network_thresholds nt ON nt.network_id = sa.network_id
			  LEFT JOIN path_thresholds pa
			         ON da.site_id IS NOT NULL AND pa.network_id IS NULL
			        AND pa.site_a_id = least(sa.site_id, da.site_id)
			        AND pa.site_b_id = greatest(sa.site_id, da.site_id)
			  LEFT JOIN path_thresholds pn
			         ON da.site_id IS NOT NULL AND pn.network_id = sa.network_id
			        AND pn.site_a_id = least(sa.site_id, da.site_id)
			        AND pn.site_b_id = greatest(sa.site_id, da.site_id)
		),
		per_site AS (
			SELECT src_site_id AS site_id, samples, ok_samples, healthy FROM graded
			UNION ALL
			SELECT dst_site_id, samples, ok_samples, healthy FROM graded
			 WHERE dst_site_id IS NOT NULL AND dst_site_id <> src_site_id
		)
		SELECT s.name,
		       coalesce(sum(p.samples), 0)::bigint,
		       coalesce(sum(p.ok_samples), 0)::bigint,
		       coalesce(sum(p.ok_samples) FILTER (WHERE p.healthy), 0)::bigint
		  FROM sites s
		  LEFT JOIN per_site p ON p.site_id = s.id
		 WHERE `+siteScopePredicate("s.id", "$3")+`
		 GROUP BY s.id, s.name
		 ORDER BY s.name`, since, excludeProbeType, networks)
	if err != nil {
		return nil, fmt.Errorf("site scores: %w", err)
	}
	defer rows.Close()
	var out []SiteScore
	for rows.Next() {
		var sc SiteScore
		if err := rows.Scan(&sc.Name, &sc.Samples, &sc.OK, &sc.HealthyOK); err != nil {
			return nil, fmt.Errorf("site scores: %w", err)
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}
