package store_test

// Pins the user_activity_days SQL: the login and session-touch upserts
// share one (identity, day) row, the account list surfaces the newest
// touch, and the monthly chart counts distinct identities per UTC month —
// none of which the httpapi fakes can exercise.

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/devalexllc/polarbeam/internal/server/store"
)

func TestUserActivityDays(t *testing.T) {
	ctx, s := newStore(t)

	id, err := s.CreateUser(ctx, "worker", "$argon2id$h", store.RoleViewer, nil)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.RecordLogin(ctx, id); err != nil {
		t.Fatalf("RecordLogin: %v", err)
	}

	var rows int
	var firstSeen time.Time
	if err := s.Pool().QueryRow(ctx, `
		SELECT count(*), max(last_seen_at) FROM user_activity_days WHERE user_id = $1`, id).
		Scan(&rows, &firstSeen); err != nil {
		t.Fatalf("count activity: %v", err)
	}
	if rows != 1 {
		t.Fatalf("activity rows after login = %d, want 1", rows)
	}

	// A touch on the same UTC day updates the row instead of adding one.
	if err := s.CreateLocalSession(ctx, id, []byte("hash-1"), "csrf", time.Now().Add(time.Hour), "$argon2id$h"); err != nil {
		t.Fatalf("CreateLocalSession: %v", err)
	}
	var sessionID uuid.UUID
	if err := s.Pool().QueryRow(ctx, `SELECT id FROM sessions WHERE user_id = $1`, id).Scan(&sessionID); err != nil {
		t.Fatalf("session id: %v", err)
	}
	time.Sleep(20 * time.Millisecond) // make last_seen_at strictly advance
	if err := s.TouchSession(ctx, sessionID); err != nil {
		t.Fatalf("TouchSession: %v", err)
	}
	var lastSeen time.Time
	if err := s.Pool().QueryRow(ctx, `
		SELECT count(*), max(last_seen_at) FROM user_activity_days WHERE user_id = $1`, id).
		Scan(&rows, &lastSeen); err != nil {
		t.Fatalf("count activity: %v", err)
	}
	if rows != 1 || !lastSeen.After(firstSeen) {
		t.Fatalf("after touch: rows=%d lastSeen=%v firstSeen=%v, want 1 row with a later timestamp", rows, lastSeen, firstSeen)
	}

	// Backdate a second day so the list and chart have something older
	// than "today" to prefer against.
	if _, err := s.Pool().Exec(ctx, `
		INSERT INTO user_activity_days (user_id, identity, day, last_seen_at)
		SELECT user_id, identity, day - 40, last_seen_at - interval '40 days'
		  FROM user_activity_days WHERE user_id = $1`, id); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	t.Run("account list carries last_active_at", func(t *testing.T) {
		accounts, _, err := s.ListUserAccounts(ctx, store.UserAccountFilter{Query: "worker", Limit: 10})
		if err != nil {
			t.Fatalf("ListUserAccounts: %v", err)
		}
		if len(accounts) != 1 {
			t.Fatalf("accounts = %d, want 1", len(accounts))
		}
		a := accounts[0]
		if a.LastActiveAt == nil || !a.LastActiveAt.Equal(lastSeen) {
			t.Errorf("LastActiveAt = %v, want %v", a.LastActiveAt, lastSeen)
		}
		if a.LastLoginAt == nil || a.LastActiveAt.Before(*a.LastLoginAt) {
			t.Errorf("LastActiveAt %v precedes LastLoginAt %v", a.LastActiveAt, a.LastLoginAt)
		}
	})

	t.Run("monthly stats count active identities", func(t *testing.T) {
		stats, err := s.MonthlyLoginStats(ctx, 12)
		if err != nil {
			t.Fatalf("MonthlyLoginStats: %v", err)
		}
		if len(stats) != 12 {
			t.Fatalf("months = %d, want 12", len(stats))
		}
		cur := stats[11]
		if cur.ActiveUsers != 1 || cur.UniqueUsers != 1 || cur.Total != 1 {
			t.Errorf("current month = %+v, want 1 active / 1 unique / 1 sign-in", cur)
		}
		// The backdated day is 40 days ago: last month or the one before,
		// with no sign-in there — active without a login is the point.
		var older int64
		for _, st := range stats[:11] {
			older += st.ActiveUsers
			if st.Total != 0 {
				t.Errorf("unexpected sign-ins in %s: %+v", st.Month.Format("2006-01"), st)
			}
		}
		if older != 1 {
			t.Errorf("active users across earlier months = %d, want 1", older)
		}
	})

	t.Run("deleted identity keeps its activity", func(t *testing.T) {
		if err := s.DeleteUser(ctx, id); err != nil {
			t.Fatalf("DeleteUser: %v", err)
		}
		accounts, _, err := s.ListUserAccounts(ctx, store.UserAccountFilter{Status: "deleted", Limit: 10})
		if err != nil {
			t.Fatalf("ListUserAccounts deleted: %v", err)
		}
		if len(accounts) != 1 || accounts[0].LastActiveAt == nil || !accounts[0].LastActiveAt.Equal(lastSeen) {
			t.Errorf("deleted accounts = %+v, want one with LastActiveAt %v", accounts, lastSeen)
		}
		stats, err := s.MonthlyLoginStats(ctx, 1)
		if err != nil || len(stats) != 1 || stats[0].ActiveUsers != 1 {
			t.Errorf("stats after delete = %+v err=%v, want 1 active", stats, err)
		}
	})

	t.Run("same-day reprovisioning keeps both accounts' activity", func(t *testing.T) {
		// A new account under the same local username (same identity)
		// signs in today: the deleted account keeps its own row, and the
		// person is still counted once.
		again, err := s.CreateUser(ctx, "worker", "$argon2id$h", store.RoleViewer, nil)
		if err != nil {
			t.Fatalf("CreateUser again: %v", err)
		}
		if err := s.RecordLogin(ctx, again); err != nil {
			t.Fatalf("RecordLogin again: %v", err)
		}
		deleted, _, err := s.ListUserAccounts(ctx, store.UserAccountFilter{Status: "deleted", Limit: 10})
		if err != nil {
			t.Fatalf("ListUserAccounts deleted: %v", err)
		}
		if len(deleted) != 1 || deleted[0].LastActiveAt == nil || !deleted[0].LastActiveAt.Equal(lastSeen) {
			t.Errorf("deleted account after reprovision = %+v, want LastActiveAt %v", deleted, lastSeen)
		}
		stats, err := s.MonthlyLoginStats(ctx, 1)
		if err != nil || len(stats) != 1 || stats[0].ActiveUsers != 1 {
			t.Errorf("stats after reprovision = %+v err=%v, want 1 active (one person)", stats, err)
		}
	})

	t.Run("migration backfill is idempotent", func(t *testing.T) {
		// Re-running 0025's INSERT...SELECT against populated tables must
		// not violate the primary key (the day already exists).
		_, err := s.Pool().Exec(ctx, `
			INSERT INTO user_activity_days (user_id, identity, day, last_seen_at)
			SELECT user_id, min(identity),
			       (occurred_at AT TIME ZONE 'UTC')::date, max(occurred_at)
			  FROM login_events
			 GROUP BY user_id, (occurred_at AT TIME ZONE 'UTC')::date
			ON CONFLICT DO NOTHING`)
		if err != nil {
			t.Fatalf("backfill re-run: %v", err)
		}
	})
}
