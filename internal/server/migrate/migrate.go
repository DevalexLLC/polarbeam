// Package migrate applies embedded SQL migrations in filename order.
// Each migration runs in its own transaction and is recorded in
// schema_migrations; a partially applied file therefore never half-commits.
//
// Files named NNNN_name.notx.sql are the exception: they run outside any
// transaction, for DDL PostgreSQL refuses inside a transaction block
// (continuous aggregate creation). A notx file must contain exactly one
// top-level statement — a multi-statement simple-query message gets an
// implicit transaction, which TimescaleDB rejects the same way — and must
// be idempotent (IF NOT EXISTS), because recording it in schema_migrations
// is a separate autocommit statement and a crash between the two must
// converge on re-run.
package migrate

import (
	"context"
	"embed"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

//go:embed sql/*.sql
var migrations embed.FS

// Querier is satisfied by both *pgx.Conn and *pgxpool.Pool.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Pending returns the embedded migrations not yet applied to the database.
// serve preflight fails when any are pending, so a reachable-but-unmigrated
// database can never present as healthy.
func Pending(ctx context.Context, q Querier) ([]string, error) {
	var haveTable bool
	if err := q.QueryRow(ctx,
		`SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&haveTable); err != nil {
		return nil, fmt.Errorf("migrate: check schema_migrations: %w", err)
	}
	names, err := embeddedNames()
	if err != nil {
		return nil, err
	}
	if !haveTable {
		return names, nil
	}
	var pending []string
	for _, name := range names {
		var applied bool
		if err := q.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE filename = $1)`,
			name).Scan(&applied); err != nil {
			return nil, fmt.Errorf("migrate: check %s: %w", name, err)
		}
		if !applied {
			pending = append(pending, name)
		}
	}
	return pending, nil
}

func embeddedNames() ([]string, error) {
	entries, err := migrations.ReadDir("sql")
	if err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// Apply runs all pending migrations. It is safe to call on every startup
// in dev; production runs it as an explicit step.
func Apply(ctx context.Context, conn *pgx.Conn) error {
	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			filename text PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("migrate: create schema_migrations: %w", err)
	}

	names, err := embeddedNames()
	if err != nil {
		return err
	}

	for _, name := range names {
		var applied bool
		if err := conn.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE filename = $1)`,
			name).Scan(&applied); err != nil {
			return fmt.Errorf("migrate: check %s: %w", name, err)
		}
		if applied {
			continue
		}
		sql, err := migrations.ReadFile("sql/" + name)
		if err != nil {
			return fmt.Errorf("migrate: read %s: %w", name, err)
		}
		if strings.HasSuffix(name, ".notx.sql") {
			if _, err := conn.Exec(ctx, string(sql)); err != nil {
				return fmt.Errorf("migrate: apply %s: %w", name, err)
			}
			if _, err := conn.Exec(ctx,
				`INSERT INTO schema_migrations (filename) VALUES ($1)`, name); err != nil {
				return fmt.Errorf("migrate: record %s: %w", name, err)
			}
			fmt.Printf("migrate: applied %s (no transaction)\n", name)
			continue
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("migrate: begin %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("migrate: apply %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (filename) VALUES ($1)`, name); err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("migrate: record %s: %w", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("migrate: commit %s: %w", name, err)
		}
		fmt.Printf("migrate: applied %s\n", name)
	}
	return nil
}
