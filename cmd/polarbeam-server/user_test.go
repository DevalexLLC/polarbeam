package main

import (
	"strings"
	"testing"
)

func TestReadNewPasswordPiped(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"polarbeam-dev\n", "polarbeam-dev", false},
		{"polarbeam-dev", "polarbeam-dev", false},     // no trailing newline
		{"polarbeam-dev\r\n", "polarbeam-dev", false}, // CRLF
		{"short\n", "", true},                         // below minimum
		{"", "", true},                                // empty stdin
		{"password with spaces\n", "password with spaces", false},
	} {
		got, err := readNewPassword(strings.NewReader(tc.in), 0, false)
		if tc.wantErr {
			if err == nil {
				t.Errorf("readNewPassword(%q): expected error, got %q", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("readNewPassword(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}
