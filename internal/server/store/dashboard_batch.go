package store

// Batched read path for the pair and target detail pages (issue #126).
// The handlers used to issue every pair query as its own round trip — the
// pair page cost ~10-12 sequential queries and target detail O(sites) — so
// these methods pipeline the same statements through pgx.Batch instead
// (the LoadAgentConfigInputs pattern). Statement text is shared with the
// single-shot methods via the pairSeriesSQL/pairSummarySQL/... builders,
// so both paths always issue byte-identical SQL; the single-shot methods
// remain as the reference implementations the parity DB suite compares
// against.
//
// The latency-source family is an input to the summary/series statements,
// so each method runs two waves: wave 1 resolves every direction's family
// (and anything else independent), wave 2 issues the family-parameterized
// statements. Each wave fully drains and closes its batch before the next
// SendBatch — a wave helper never holds two open batch results, which
// would pin two pool connections and deadlock a size-1 pool.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// DirectionKey is one (source agents → destination targets) read direction.
// The pair page queries two (site A→B, site B→A); target detail queries one
// per source site, all aimed at the single target ID.
type DirectionKey struct {
	SrcAgents  []uuid.UUID
	DstTargets []uuid.UUID
}

// DirectionSummary is one direction's pair-page summary: the windowed
// aggregate (LatencySource set to the family chosen for this direction)
// plus the latest per-series rows inside the staleness horizon.
type DirectionSummary struct {
	Summary PairSummaryRow
	Latest  []MatrixRow
}

// DirectionSeries is one direction's bucketed chart series and the timing
// family it was computed from.
type DirectionSeries struct {
	LatencySource string
	Points        []SeriesBucket
}

// PairDirectionSummaries computes PairSummary + DirectionLatest for every
// direction in two round trips total, choosing each direction's latency
// family exactly once. out[i] corresponds to dirs[i]. A direction whose
// SrcAgents is empty (a plane filtered away) is still queried, matching the
// single-shot path — its NULL aggregates are how the JSON renders "empty".
func (s *Store) PairDirectionSummaries(ctx context.Context, dirs []DirectionKey, window time.Duration, source Source, horizon time.Duration) ([]DirectionSummary, error) {
	if len(dirs) == 0 {
		return nil, nil
	}
	out := make([]DirectionSummary, len(dirs))
	families, err := s.pairSummaryWave1(ctx, dirs, window, source, horizon, out)
	if err != nil {
		return nil, err
	}
	if err := s.pairSummaryWave2(ctx, dirs, window, source, families, out); err != nil {
		return nil, err
	}
	return out, nil
}

// pairSummaryWave1 batches every direction's latency-source counts and
// DirectionLatest rows: the two statements that do not depend on the chosen
// family. It fills out[i].Latest and returns each direction's family.
func (s *Store) pairSummaryWave1(ctx context.Context, dirs []DirectionKey, window time.Duration, source Source, horizon time.Duration, out []DirectionSummary) ([]string, error) {
	batch := &pgx.Batch{}
	for _, d := range dirs {
		batch.Queue(pairLatencySourceSQL(source), d.SrcAgents, d.DstTargets, window)
		batch.Queue(directionLatestSQL, d.SrcAgents, d.DstTargets, horizon)
	}
	res := s.pool.SendBatch(ctx, batch)
	defer res.Close()
	families := make([]string, len(dirs))
	for i := range dirs {
		rows, err := res.Query()
		if err == nil {
			var counts map[string]int64
			counts, err = scanLatencyCounts(rows)
			families[i] = chooseLatencySource(counts)
		}
		if err != nil {
			return nil, fmt.Errorf("pair latency source (direction %d): %w", i, err)
		}
		rows, err = res.Query()
		if err == nil {
			out[i].Latest, err = scanDirectionLatest(rows)
		}
		if err != nil {
			return nil, fmt.Errorf("direction latest (direction %d): %w", i, err)
		}
	}
	return families, nil
}

// pairSummaryWave2 batches every direction's family-parameterized summary
// statement, filling out[i].Summary.
func (s *Store) pairSummaryWave2(ctx context.Context, dirs []DirectionKey, window time.Duration, source Source, families []string, out []DirectionSummary) error {
	batch := &pgx.Batch{}
	for i, d := range dirs {
		batch.Queue(pairSummarySQL(source), d.SrcAgents, d.DstTargets, window, families[i])
	}
	res := s.pool.SendBatch(ctx, batch)
	defer res.Close()
	for i := range dirs {
		p, err := scanPairSummaryRow(res.QueryRow())
		if err != nil {
			return fmt.Errorf("pair summary (direction %d): %w", i, err)
		}
		p.LatencySource = families[i]
		out[i].Summary = *p
	}
	return nil
}

// PairDirectionSeries computes PairLatencySource + PairSeries for every
// direction in two round trips total. out[i] corresponds to dirs[i]; its
// LatencySource is the family the points were filtered by.
func (s *Store) PairDirectionSeries(ctx context.Context, dirs []DirectionKey, bucket, window time.Duration, source Source) ([]DirectionSeries, error) {
	if len(dirs) == 0 {
		return nil, nil
	}
	families, err := s.pairSeriesWave1(ctx, dirs, window, source)
	if err != nil {
		return nil, err
	}
	return s.pairSeriesWave2(ctx, dirs, bucket, window, source, families)
}

func (s *Store) pairSeriesWave1(ctx context.Context, dirs []DirectionKey, window time.Duration, source Source) ([]string, error) {
	batch := &pgx.Batch{}
	for _, d := range dirs {
		batch.Queue(pairLatencySourceSQL(source), d.SrcAgents, d.DstTargets, window)
	}
	res := s.pool.SendBatch(ctx, batch)
	defer res.Close()
	families := make([]string, len(dirs))
	for i := range dirs {
		rows, err := res.Query()
		if err == nil {
			var counts map[string]int64
			counts, err = scanLatencyCounts(rows)
			families[i] = chooseLatencySource(counts)
		}
		if err != nil {
			return nil, fmt.Errorf("pair latency source (direction %d): %w", i, err)
		}
	}
	return families, nil
}

func (s *Store) pairSeriesWave2(ctx context.Context, dirs []DirectionKey, bucket, window time.Duration, source Source, families []string) ([]DirectionSeries, error) {
	batch := &pgx.Batch{}
	for i, d := range dirs {
		batch.Queue(pairSeriesSQL(source), bucket, d.SrcAgents, d.DstTargets, window, families[i])
	}
	res := s.pool.SendBatch(ctx, batch)
	defer res.Close()
	out := make([]DirectionSeries, len(dirs))
	for i := range dirs {
		out[i].LatencySource = families[i]
		rows, err := res.Query()
		if err == nil {
			out[i].Points, err = scanSeriesBuckets(rows)
		}
		if err != nil {
			return nil, fmt.Errorf("pair series (direction %d): %w", i, err)
		}
	}
	return out, nil
}

// SiteEndpointsBatch resolves several site names in two round trips total.
// out[i] corresponds to names[i] and is nil when that site does not exist
// or is not visible to the caller's scope — the same (nil, nil) 404 signal
// SiteEndpoints returns, byte-identical for both cases so a tenant cannot
// probe for other tenants' site names.
func (s *Store) SiteEndpointsBatch(ctx context.Context, names []string, networks []uuid.UUID) ([]*SiteEndpoints, error) {
	if len(names) == 0 {
		return nil, nil
	}
	out, err := s.siteEndpointsWave1(ctx, names)
	if err != nil {
		return nil, err
	}
	if err := s.siteEndpointsWave2(ctx, names, networks, out); err != nil {
		return nil, err
	}
	return out, nil
}

// siteEndpointsWave1 batches the site-row lookups; unknown names yield nil
// entries.
func (s *Store) siteEndpointsWave1(ctx context.Context, names []string) ([]*SiteEndpoints, error) {
	batch := &pgx.Batch{}
	for _, name := range names {
		batch.Queue(siteRowSQL, name)
	}
	res := s.pool.SendBatch(ctx, batch)
	defer res.Close()
	out := make([]*SiteEndpoints, len(names))
	for i, name := range names {
		var ep SiteEndpoints
		err := res.QueryRow().Scan(&ep.ID, &ep.Name, &ep.DisplayName, &ep.Location, &ep.Latitude, &ep.Longitude)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("site %q: %w", name, err)
		}
		out[i] = &ep
	}
	return out, nil
}

// siteEndpointsWave2 batches, per found site, the scope-visibility check
// (only under a scope, mirroring the single-shot path) and the endpoint
// triples. An invisible site's entry becomes nil, but its already-queued
// triples result is still drained in order — batch results must be consumed
// positionally. The triples statement carries the network filter itself, so
// the discarded rows are ones the caller was allowed to see anyway.
func (s *Store) siteEndpointsWave2(ctx context.Context, names []string, networks []uuid.UUID, out []*SiteEndpoints) error {
	batch := &pgx.Batch{}
	for _, ep := range out {
		if ep == nil {
			continue
		}
		if networks != nil {
			batch.Queue(siteVisibleSQL, ep.ID, networks)
		}
		batch.Queue(siteTriplesSQL, ep.ID, networks)
	}
	if batch.Len() == 0 {
		return nil
	}
	res := s.pool.SendBatch(ctx, batch)
	defer res.Close()
	for i, ep := range out {
		if ep == nil {
			continue
		}
		visible := true
		if networks != nil {
			if err := res.QueryRow().Scan(&visible); err != nil {
				return fmt.Errorf("site %q: %w", names[i], err)
			}
		}
		rows, err := res.Query()
		if err == nil {
			err = scanSiteTriples(rows, ep)
		}
		if err != nil {
			return fmt.Errorf("site %q endpoints: %w", names[i], err)
		}
		if !visible {
			out[i] = nil
		}
	}
	return nil
}
