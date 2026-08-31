package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// rowScanner abstracts pgx.Row and pgx.Rows so the same scan helpers serve
// both the paged inventory queries and their single-row Get* counterparts.
type rowScanner interface {
	Scan(dest ...any) error
}

// pageBounds is the shared page contract for every paginated inventory
// query: 1..100 rows, non-negative offset. name is the query's error noun
// ("agent inventory", "site config", ...).
func pageBounds(name string, limit, offset int) error {
	if limit < 1 || limit > 100 || offset < 0 {
		return invalidf("invalid %s page", name)
	}
	return nil
}

// orderColumn is one allowlisted sort key: the SQL expression to order by
// plus an optional suffix placed after the direction (" NULLS LAST").
type orderColumn struct {
	expr   string
	suffix string
}

// orderAllowlist maps caller-supplied sort keys to ORDER BY clauses as
// data. Every clause ends with a direction-matched tie-break on tieExpr so
// paging stays stable under duplicate sort values.
type orderAllowlist struct {
	name    string // error noun, matching pageBounds
	tieExpr string // "id", or table-qualified where the query needs it
	columns map[string]orderColumn
}

// clause validates order and sortName against the allowlist and returns
// the composed ORDER BY body: expr + direction + suffix, tie + direction.
func (a orderAllowlist) clause(sortName, order string) (string, error) {
	var direction string
	switch order {
	case "asc":
		direction = " ASC"
	case "desc":
		direction = " DESC"
	default:
		return "", invalidf("%s order must be asc or desc", a.name)
	}
	col, ok := a.columns[sortName]
	if !ok {
		return "", invalidf("unknown %s sort %q", a.name, sortName)
	}
	return col.expr + direction + col.suffix + ", " + a.tieExpr + direction, nil
}

// pagedSpec drives runPaged, the one pagination implementation behind the
// inventory Query* reads. pageSQL must take limit and offset as its final
// two placeholders; aggregate columns riding along on every row (totals,
// summary counts) are captured by the scan closure, so the helper needs no
// knowledge of them.
type pagedSpec[T any] struct {
	queryErr    string // wrap for the page query, e.g. "query agents"
	scanErr     string // wrap for a row scan, e.g. "scan queried agent"
	fallbackErr string // wrap for the fallback, e.g. "summarize agents"

	pageSQL string
	args    []any // filter args; runPaged appends limit and offset

	scan func(row rowScanner) (T, error)

	// fallback recomputes the window aggregates when the page has no rows
	// to carry them (past-the-end offsets, or an empty filtered set when
	// fallbackAlways is on). Empty fallbackSQL disables it.
	fallbackSQL    string
	fallbackDest   []any
	fallbackAlways bool // run at offset 0 too, not only past the end
}

// runPaged executes one page query, scans it into a non-nil slice, and
// runs the aggregate fallback when the page comes back empty.
func runPaged[T any](ctx context.Context, pool *pgxpool.Pool, limit, offset int, sp pagedSpec[T]) ([]T, error) {
	rows, err := pool.Query(ctx, sp.pageSQL, append(sp.args, limit, offset)...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", sp.queryErr, err)
	}
	defer rows.Close()
	list := make([]T, 0, limit)
	for rows.Next() {
		item, err := sp.scan(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", sp.scanErr, err)
		}
		list = append(list, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(list) == 0 && sp.fallbackSQL != "" && (offset > 0 || sp.fallbackAlways) {
		if err := pool.QueryRow(ctx, sp.fallbackSQL, sp.args...).Scan(sp.fallbackDest...); err != nil {
			return nil, fmt.Errorf("%s: %w", sp.fallbackErr, err)
		}
	}
	return list, nil
}
