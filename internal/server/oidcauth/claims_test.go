package oidcauth

import (
	"errors"
	"strings"
	"testing"
)

func TestMapClaims(t *testing.T) {
	admins := []string{"polarbeam-admins", "sre"}
	cases := []struct {
		name     string
		subject  string
		all      map[string]any
		wantRole string
		wantUser string
		wantErr  string
	}{
		{name: "array claim with admin match", subject: "s1",
			all:      map[string]any{"preferred_username": "alice", "groups": []any{"dev", "polarbeam-admins"}},
			wantUser: "alice", wantRole: "admin"},
		{name: "array claim no match", subject: "s1",
			all:      map[string]any{"preferred_username": "bob", "groups": []any{"dev"}},
			wantUser: "bob", wantRole: "viewer"},
		{name: "string claim match", subject: "s1",
			all:      map[string]any{"preferred_username": "carol", "groups": "sre"},
			wantUser: "carol", wantRole: "admin"},
		{name: "string claim no match", subject: "s1",
			all:      map[string]any{"preferred_username": "dan", "groups": "marketing"},
			wantUser: "dan", wantRole: "viewer"},
		{name: "absent role claim is viewer", subject: "s1",
			all:      map[string]any{"preferred_username": "erin"},
			wantUser: "erin", wantRole: "viewer"},
		{name: "non-string role values are viewer", subject: "s1",
			all:      map[string]any{"preferred_username": "frank", "groups": []any{42, true}},
			wantUser: "frank", wantRole: "viewer"},
		{name: "empty subject", subject: "",
			all:     map[string]any{"preferred_username": "x"},
			wantErr: "empty subject"},
		{name: "missing username claim", subject: "s1",
			all:     map[string]any{"groups": []any{"polarbeam-admins"}},
			wantErr: `missing username claim "preferred_username"`},
		{name: "empty username claim", subject: "s1",
			all:     map[string]any{"preferred_username": ""},
			wantErr: `missing username claim "preferred_username"`},
		{name: "non-string username claim", subject: "s1",
			all:     map[string]any{"preferred_username": 7},
			wantErr: `missing username claim "preferred_username"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mapClaims("preferred_username", "groups", admins, tc.subject, tc.all)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want mention of %q", err, tc.wantErr)
				}
				var ce *ClaimsError
				if !errors.As(err, &ce) {
					t.Errorf("mapping errors must be *ClaimsError, got %T", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("mapClaims: %v", err)
			}
			if got.Subject != tc.subject || got.Username != tc.wantUser || got.Role != tc.wantRole {
				t.Errorf("claims = %+v, want user %q role %q", got, tc.wantUser, tc.wantRole)
			}
		})
	}
}

func TestMapClaimsNoAdminValues(t *testing.T) {
	// With no admin_values configured nothing can elevate, whatever the
	// claim carries.
	got, err := mapClaims("sub", "groups", nil, "s1",
		map[string]any{"sub": "s1", "groups": []any{"anything"}})
	if err != nil {
		t.Fatalf("mapClaims: %v", err)
	}
	if got.Role != "viewer" {
		t.Errorf("role = %q, want viewer floor", got.Role)
	}
}
