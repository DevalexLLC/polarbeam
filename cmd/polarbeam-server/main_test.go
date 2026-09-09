package main

import (
	"reflect"
	"testing"
	"time"
)

func TestTokenCreateProblems(t *testing.T) {
	cases := []struct {
		name string
		site string
		ttl  time.Duration
		want []string
	}{
		{"valid", "site-a", 24 * time.Hour, nil},
		{"zero ttl", "site-a", 0, []string{"--ttl must be positive"}},
		{"negative ttl", "site-a", -time.Hour, []string{"--ttl must be positive"}},
		{"empty site", "", time.Hour, []string{"--site is required"}},
		{"empty site and negative ttl", "", -time.Hour, []string{"--site is required", "--ttl must be positive"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tokenCreateProblems(tc.site, tc.ttl)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("tokenCreateProblems(%q, %v) = %v, want %v", tc.site, tc.ttl, got, tc.want)
			}
		})
	}
}
