package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/devalexllc/polarbeam/internal/server/dbtest"
	"github.com/devalexllc/polarbeam/internal/server/store"
)

// Connect stamps session defaults on every pooled connection: an
// application_name for pg_stat_activity/pg_stat_statements attribution
// (a URL-supplied one wins) and an idle-in-transaction timeout so a stuck
// goroutine can never hold chunk locks against the retention and
// columnstore jobs indefinitely.
func TestConnectSessionDefaults(t *testing.T) {
	t.Parallel()
	url := dbtest.Migrated(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)

	settings := func(s *store.Store) (app, idle string) {
		t.Helper()
		if err := s.Pool().QueryRow(ctx,
			`SELECT current_setting('application_name'), current_setting('idle_in_transaction_session_timeout')`).
			Scan(&app, &idle); err != nil {
			t.Fatalf("read settings: %v", err)
		}
		return app, idle
	}

	s, err := store.Connect(ctx, url, 10*time.Second, 0)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(s.Close)
	app, idle := settings(s)
	if app != "polarbeam-server" {
		t.Errorf("application_name = %q, want polarbeam-server", app)
	}
	if idle != "2min" {
		t.Errorf("idle_in_transaction_session_timeout = %q, want 2min", idle)
	}

	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	named, err := store.Connect(ctx, url+sep+"application_name=custom-name", 10*time.Second, 0)
	if err != nil {
		t.Fatalf("Connect with application_name: %v", err)
	}
	t.Cleanup(named.Close)
	if app, _ := settings(named); app != "custom-name" {
		t.Errorf("URL application_name = %q, want custom-name (the URL must win)", app)
	}
}
