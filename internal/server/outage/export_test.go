package outage

import (
	"context"
	"time"
)

// SweepOnce exposes one agent_offline sweep pass to DB-backed tests, which
// pin the min-interval attribution SQL the pure decideOffline tests cannot
// reach. Not part of the public API.
func SweepOnce(ctx context.Context, db DB, now time.Time) error {
	return sweepOnce(ctx, db, now)
}
