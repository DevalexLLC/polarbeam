package targetadmin

import (
	"strings"
	"testing"
)

var cliFields = FieldNames{Name: "--name", Address: "--address", URL: "--url", Port: "--port"}
var apiFields = FieldNames{Name: "name", Address: "address", URL: "url", Port: "port"}

func TestValidate(t *testing.T) {
	cases := []struct {
		name, address, url string
		port               int64
		problems           int
	}{
		{"dns1", "10.0.0.1", "", 0, 0}, // port 0 is the "unset" sentinel
		{"web", "", "https://example.com", 0, 0},
		{"tls1", "10.0.0.1", "", 65535, 0},
		{"tls1", "10.0.0.1", "", 443, 0},
		{"bad", "10.0.0.1", "", -1, 1},
		{"bad", "10.0.0.1", "", 65536, 1},
		{"bad", "10.0.0.1", "", 4294967296, 1}, // would truncate to 0 as int32
		{"", "10.0.0.1", "", 443, 1},
		{"   ", "10.0.0.1", "", 443, 1},
		{"noaddr", "", "", 443, 1},
		{"noaddr", "   ", "", 443, 1},
		{"", "", "", 99999, 3}, // every problem reported, not just the first
	}
	for _, c := range cases {
		if got := Validate(c.name, c.address, c.url, c.port, apiFields); len(got) != c.problems {
			t.Errorf("Validate(%q, %q, %q, %d) = %v, want %d problem(s)",
				c.name, c.address, c.url, c.port, got, c.problems)
		}
	}
	got := Validate("t", "10.0.0.1", "", 99999, cliFields)
	if len(got) != 1 || !strings.Contains(got[0], "--port must be between 0 and 65535, got 99999") {
		t.Errorf("Validate with CLI fields = %v, want problem naming --port with the value", got)
	}
	got = Validate("", "", "", 70000, apiFields)
	joined := strings.Join(got, "; ")
	for _, want := range []string{"name is required", "address or url is required", "port must be between"} {
		if !strings.Contains(joined, want) {
			t.Errorf("Validate with API fields = %q, missing %q", joined, want)
		}
	}
}
