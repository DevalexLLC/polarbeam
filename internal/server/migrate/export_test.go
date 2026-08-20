package migrate

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// ApplyThrough exposes partial application to upgrade-path tests, which
// seed old-shape rows between two migration points before applying the
// rest. Not part of the public API.
func ApplyThrough(ctx context.Context, conn *pgx.Conn, stopAfter string) error {
	return apply(ctx, conn, stopAfter)
}
