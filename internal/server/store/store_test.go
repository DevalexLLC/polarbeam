package store

import "testing"

func TestURLSetsPoolMaxConns(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"postgres://u:p@h:5432/db", false},
		{"postgres://u:p@h:5432/db?pool_max_conns=8", true},
		{"postgres://u:p@h:5432/db?pool%5Fmax%5Fconns=8", true}, // percent-encoded still counts
		{"postgres://u:pool_max_conns=9@h:5432/db", false},      // inside the password: not an option
		{"host=h dbname=db pool_max_conns=8", true},             // DSN keyword form
		{"host=h dbname=db password=pool_max_conns=9", false},   // inside a DSN value: not an option
		{"host=h dbname=db", false},
	} {
		if got := urlSetsPoolMaxConns(tc.url); got != tc.want {
			t.Errorf("urlSetsPoolMaxConns(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}
