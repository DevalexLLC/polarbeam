package main

import (
	"reflect"
	"testing"

	"github.com/google/uuid"
)

func TestAgentRevokeArgs(t *testing.T) {
	id := uuid.New()
	cases := []struct {
		name          string
		agent, serial string
		wantAgent     uuid.UUID
		wantSerial    string // "" = agent-wide form
		want          []string
	}{
		{"agent", id.String(), "", id, "", nil},
		{"serial", "", "123456789012345678901234567890", uuid.Nil, "123456789012345678901234567890", nil},
		{"neither", "", "", uuid.Nil, "", []string{"one of --agent or --serial is required"}},
		{"both", id.String(), "1", uuid.Nil, "", []string{"--agent and --serial are mutually exclusive"}},
		{"bad agent", "nyc-1", "", uuid.Nil, "", []string{`--agent "nyc-1" is not a UUID`}},
		{"hex serial", "", "0x1f", uuid.Nil, "", []string{`--serial "0x1f" is not a non-negative decimal integer`}},
		{"negative serial", "", "-5", uuid.Nil, "", []string{`--serial "-5" is not a non-negative decimal integer`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent, serial, problems := agentRevokeArgs(tc.agent, tc.serial)
			if !reflect.DeepEqual(problems, tc.want) {
				t.Fatalf("problems = %v, want %v", problems, tc.want)
			}
			if tc.want != nil {
				return
			}
			if agent != tc.wantAgent {
				t.Errorf("agent = %s, want %s", agent, tc.wantAgent)
			}
			switch {
			case tc.wantSerial == "" && serial != nil:
				t.Errorf("serial = %s, want agent-wide form", serial)
			case tc.wantSerial != "" && (serial == nil || serial.String() != tc.wantSerial):
				t.Errorf("serial = %v, want %s", serial, tc.wantSerial)
			}
		})
	}
}
