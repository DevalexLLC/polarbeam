package oidcauth

import (
	"errors"
	"strings"
	"testing"

	"github.com/devalexllc/polarbeam/internal/server/store"
)

func TestMapClaims(t *testing.T) {
	admins := []string{"polarbeam-admins", "sre"}
	cases := []struct {
		name     string
		issuer   string
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
		{name: "empty issuer", issuer: "-", subject: "s1",
			all:     map[string]any{"preferred_username": "x"},
			wantErr: "empty issuer"},
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
			issuer := "https://idp.example/realms/x"
			if tc.issuer == "-" { // sentinel: the empty-issuer case
				issuer = ""
			}
			got, err := mapClaims("preferred_username", "groups", admins, nil, "viewer", issuer, tc.subject, tc.all)
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
			if got.Issuer != issuer || got.Subject != tc.subject || got.Username != tc.wantUser || got.Role != tc.wantRole {
				t.Errorf("claims = %+v, want issuer %q user %q role %q", got, issuer, tc.wantUser, tc.wantRole)
			}
		})
	}
}

func TestMapClaimsNoAdminValues(t *testing.T) {
	// With no admin_values configured nothing can elevate, whatever the
	// claim carries.
	got, err := mapClaims("sub", "groups", nil, nil, "viewer", "https://idp.example/realms/x", "s1",
		map[string]any{"sub": "s1", "groups": []any{"anything"}})
	if err != nil {
		t.Fatalf("mapClaims: %v", err)
	}
	if got.Role != "viewer" {
		t.Errorf("role = %q, want viewer floor", got.Role)
	}
}

func TestMapClaimsRoleRules(t *testing.T) {
	admins := []string{"polarbeam-admins"}
	rules := []store.OIDCRoleRule{
		{Value: "tenant-a-admins", Role: store.RoleNetworkAdmin, Networks: []string{"tenant-a"}},
		{Value: "tenant-b-admins", Role: store.RoleNetworkAdmin, Networks: []string{"tenant-b"}},
		{Value: "tenant-a-viewers", Role: store.RoleNetworkViewer, Networks: []string{"tenant-a"}},
	}
	claims := func(groups ...any) map[string]any {
		return map[string]any{"preferred_username": "u", "groups": groups}
	}
	run := func(t *testing.T, unmatched string, all map[string]any) (*Claims, error) {
		t.Helper()
		return mapClaims("preferred_username", "groups", admins, rules, unmatched,
			"https://idp.example/realms/x", "s1", all)
	}

	t.Run("rule match grants scoped role", func(t *testing.T) {
		got, err := run(t, "deny", claims("tenant-a-admins"))
		if err != nil {
			t.Fatalf("mapClaims: %v", err)
		}
		if got.Role != store.RoleNetworkAdmin || len(got.Networks) != 1 || got.Networks[0] != "tenant-a" {
			t.Errorf("claims = %+v, want network_admin over [tenant-a]", got)
		}
	})
	t.Run("admin_values beats rules", func(t *testing.T) {
		got, err := run(t, "deny", claims("polarbeam-admins", "tenant-a-admins"))
		if err != nil {
			t.Fatalf("mapClaims: %v", err)
		}
		if got.Role != store.RoleAdmin || got.Networks != nil {
			t.Errorf("claims = %+v, want unscoped admin", got)
		}
	})
	t.Run("same-role rules union networks", func(t *testing.T) {
		got, err := run(t, "deny", claims("tenant-a-admins", "tenant-b-admins"))
		if err != nil {
			t.Fatalf("mapClaims: %v", err)
		}
		if got.Role != store.RoleNetworkAdmin || len(got.Networks) != 2 ||
			got.Networks[0] != "tenant-a" || got.Networks[1] != "tenant-b" {
			t.Errorf("claims = %+v, want network_admin over [tenant-a tenant-b]", got)
		}
	})
	t.Run("stronger role wins over weaker", func(t *testing.T) {
		got, err := run(t, "deny", claims("tenant-a-viewers", "tenant-a-admins"))
		if err != nil {
			t.Fatalf("mapClaims: %v", err)
		}
		if got.Role != store.RoleNetworkAdmin {
			t.Errorf("role = %q, want network_admin (admin rule outranks viewer rule)", got.Role)
		}
	})
	t.Run("unmatched with deny refuses", func(t *testing.T) {
		_, err := run(t, "deny", claims("unrelated"))
		var denied *AccessDeniedError
		if !errors.As(err, &denied) {
			t.Fatalf("err = %v, want *AccessDeniedError", err)
		}
	})
	t.Run("unmatched with viewer keeps the floor", func(t *testing.T) {
		got, err := run(t, "viewer", claims("unrelated"))
		if err != nil {
			t.Fatalf("mapClaims: %v", err)
		}
		if got.Role != store.RoleViewer || got.Networks != nil {
			t.Errorf("claims = %+v, want unscoped viewer", got)
		}
	})
	t.Run("absent claim with deny refuses", func(t *testing.T) {
		_, err := run(t, "deny", map[string]any{"preferred_username": "u"})
		var denied *AccessDeniedError
		if !errors.As(err, &denied) {
			t.Fatalf("err = %v, want *AccessDeniedError", err)
		}
	})
}
