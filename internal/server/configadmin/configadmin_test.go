package configadmin

import (
	"strings"
	"testing"
)

var cliSiteFields = SiteFields{Name: "--name", Lat: "--lat", Lon: "--lon"}
var apiSiteFields = SiteFields{Name: "name", Lat: "latitude", Lon: "longitude"}

var cliNetworkFields = NetworkFields{Name: "--name"}
var apiNetworkFields = NetworkFields{Name: "name"}

var cliTargetFields = TargetFields{Name: "--name", Address: "--address", URL: "--url", Port: "--port"}
var apiTargetFields = TargetFields{Name: "name", Address: "address", URL: "url", Port: "port"}

func TestValidateSiteName(t *testing.T) {
	cases := []struct {
		name     string
		problems int
	}{
		{"nyc", 0},
		{"a", 0},
		{"", 1},
		{"   ", 1},
		{"\t\n", 1},
	}
	for _, c := range cases {
		if got := ValidateSiteName(c.name, apiSiteFields); len(got) != c.problems {
			t.Errorf("ValidateSiteName(%q) = %v, want %d problem(s)", c.name, got, c.problems)
		}
	}
	if got := ValidateSiteName("", cliSiteFields); len(got) != 1 || !strings.Contains(got[0], "--name") {
		t.Errorf("ValidateSiteName with CLI fields = %v, want problem naming --name", got)
	}
}

func TestValidateSiteCoords(t *testing.T) {
	cases := []struct {
		lat, lon float64
		problems int
	}{
		{0, 0, 0}, // 0,0 is a real coordinate
		{40.7128, -74.0060, 0},
		{-90, -180, 0},
		{90, 180, 0},
		{-90.0001, 0, 1},
		{90.0001, 0, 1},
		{0, -180.0001, 1},
		{0, 180.0001, 1},
		{91, 181, 2}, // every problem reported, not just the first
	}
	for _, c := range cases {
		if got := ValidateSiteCoords(c.lat, c.lon, apiSiteFields); len(got) != c.problems {
			t.Errorf("ValidateSiteCoords(%g, %g) = %v, want %d problem(s)", c.lat, c.lon, got, c.problems)
		}
	}
	got := ValidateSiteCoords(-91, 0, cliSiteFields)
	if len(got) != 1 || !strings.Contains(got[0], "--lat") {
		t.Errorf("ValidateSiteCoords with CLI fields = %v, want problem naming --lat", got)
	}
	got = ValidateSiteCoords(0, 999, apiSiteFields)
	if len(got) != 1 || !strings.Contains(got[0], "longitude") {
		t.Errorf("ValidateSiteCoords with API fields = %v, want problem naming longitude", got)
	}
}

func TestValidateNetworkName(t *testing.T) {
	cases := []struct {
		name     string
		problems int
	}{
		{"mgmt", 0},
		{"default", 0},
		{"a", 0},
		{"", 1},
		{"   ", 1},
		{"\t\n", 1},
	}
	for _, c := range cases {
		if got := ValidateNetworkName(c.name, apiNetworkFields); len(got) != c.problems {
			t.Errorf("ValidateNetworkName(%q) = %v, want %d problem(s)", c.name, got, c.problems)
		}
	}
	if got := ValidateNetworkName("", cliNetworkFields); len(got) != 1 || !strings.Contains(got[0], "--name") {
		t.Errorf("ValidateNetworkName with CLI fields = %v, want problem naming --name", got)
	}
}

func TestValidateTarget(t *testing.T) {
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
		if got := ValidateTarget(c.name, c.address, c.url, c.port, apiTargetFields); len(got) != c.problems {
			t.Errorf("ValidateTarget(%q, %q, %q, %d) = %v, want %d problem(s)",
				c.name, c.address, c.url, c.port, got, c.problems)
		}
	}
	got := ValidateTarget("t", "10.0.0.1", "", 99999, cliTargetFields)
	if len(got) != 1 || !strings.Contains(got[0], "--port must be between 0 and 65535, got 99999") {
		t.Errorf("ValidateTarget with CLI fields = %v, want problem naming --port with the value", got)
	}
	got = ValidateTarget("", "", "", 70000, apiTargetFields)
	joined := strings.Join(got, "; ")
	for _, want := range []string{"name is required", "address or url is required", "port must be between"} {
		if !strings.Contains(joined, want) {
			t.Errorf("ValidateTarget with API fields = %q, missing %q", joined, want)
		}
	}
}
