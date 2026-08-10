package siteadmin

import (
	"strings"
	"testing"
)

var cliFields = FieldNames{Name: "--name", Lat: "--lat", Lon: "--lon"}
var apiFields = FieldNames{Name: "name", Lat: "latitude", Lon: "longitude"}

func TestValidateName(t *testing.T) {
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
		if got := ValidateName(c.name, apiFields); len(got) != c.problems {
			t.Errorf("ValidateName(%q) = %v, want %d problem(s)", c.name, got, c.problems)
		}
	}
	if got := ValidateName("", cliFields); len(got) != 1 || !strings.Contains(got[0], "--name") {
		t.Errorf("ValidateName with CLI fields = %v, want problem naming --name", got)
	}
}

func TestValidateCoords(t *testing.T) {
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
		if got := ValidateCoords(c.lat, c.lon, apiFields); len(got) != c.problems {
			t.Errorf("ValidateCoords(%g, %g) = %v, want %d problem(s)", c.lat, c.lon, got, c.problems)
		}
	}
	got := ValidateCoords(-91, 0, cliFields)
	if len(got) != 1 || !strings.Contains(got[0], "--lat") {
		t.Errorf("ValidateCoords with CLI fields = %v, want problem naming --lat", got)
	}
	got = ValidateCoords(0, 999, apiFields)
	if len(got) != 1 || !strings.Contains(got[0], "longitude") {
		t.Errorf("ValidateCoords with API fields = %v, want problem naming longitude", got)
	}
}
