package networkadmin

import (
	"strings"
	"testing"
)

var cliFields = FieldNames{Name: "--name"}
var apiFields = FieldNames{Name: "name"}

func TestValidateName(t *testing.T) {
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
		if got := ValidateName(c.name, apiFields); len(got) != c.problems {
			t.Errorf("ValidateName(%q) = %v, want %d problem(s)", c.name, got, c.problems)
		}
	}
	if got := ValidateName("", cliFields); len(got) != 1 || !strings.Contains(got[0], "--name") {
		t.Errorf("ValidateName with CLI fields = %v, want problem naming --name", got)
	}
}
