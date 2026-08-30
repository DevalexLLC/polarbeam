package store

import (
	"errors"
	"testing"
)

// fixtureSort exercises every composition rule the real allowlists rely
// on: plain expressions, a direction suffix, and a qualified tie-break.
var fixtureSort = orderAllowlist{
	name:    "fixture",
	tieExpr: "f.id",
	columns: map[string]orderColumn{
		"name": {expr: "lower(name)"},
		"seen": {expr: "seen_at", suffix: " NULLS LAST"},
	},
}

func TestOrderAllowlistClause(t *testing.T) {
	cases := []struct {
		list  orderAllowlist
		sort  string
		order string
		want  string
	}{
		{fixtureSort, "name", "asc", "lower(name) ASC, f.id ASC"},
		{fixtureSort, "name", "desc", "lower(name) DESC, f.id DESC"},
		{fixtureSort, "seen", "desc", "seen_at DESC NULLS LAST, f.id DESC"},
		{siteConfigSort, "display_name", "asc", "lower(display_name) ASC, id ASC"},
		{siteConfigSort, "agents", "desc", "agent_count DESC, id DESC"},
		{targetConfigSort, "network", "asc", "lower(network) ASC, id ASC"},
		{targetConfigSort, "created", "desc", "created_at DESC, id DESC"},
	}
	for _, tc := range cases {
		got, err := tc.list.clause(tc.sort, tc.order)
		if err != nil {
			t.Errorf("clause(%q, %q): unexpected error %v", tc.sort, tc.order, err)
			continue
		}
		if got != tc.want {
			t.Errorf("clause(%q, %q) = %q, want %q", tc.sort, tc.order, got, tc.want)
		}
	}
}

func TestOrderAllowlistClauseErrors(t *testing.T) {
	cases := []struct {
		sort  string
		order string
		want  string
	}{
		{"name", "sideways", "fixture order must be asc or desc"},
		{"name", "", "fixture order must be asc or desc"},
		{"bogus", "asc", `unknown fixture sort "bogus"`},
		{"", "asc", `unknown fixture sort ""`},
	}
	for _, tc := range cases {
		_, err := fixtureSort.clause(tc.sort, tc.order)
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("clause(%q, %q): error %v, want ErrInvalid", tc.sort, tc.order, err)
			continue
		}
		if err.Error() != tc.want {
			t.Errorf("clause(%q, %q) error = %q, want %q", tc.sort, tc.order, err.Error(), tc.want)
		}
	}
}

func TestPageBounds(t *testing.T) {
	cases := []struct {
		limit, offset int
		ok            bool
	}{
		{1, 0, true},
		{100, 0, true},
		{50, 1000, true},
		{0, 0, false},
		{101, 0, false},
		{-1, 0, false},
		{1, -1, false},
	}
	for _, tc := range cases {
		err := pageBounds("fixture", tc.limit, tc.offset)
		if tc.ok {
			if err != nil {
				t.Errorf("pageBounds(%d, %d): unexpected error %v", tc.limit, tc.offset, err)
			}
			continue
		}
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("pageBounds(%d, %d): error %v, want ErrInvalid", tc.limit, tc.offset, err)
		} else if err.Error() != "invalid fixture page" {
			t.Errorf("pageBounds(%d, %d) error = %q", tc.limit, tc.offset, err.Error())
		}
	}
}
