package outage

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDecideOffline(t *testing.T) {
	now := time.Unix(10_000, 0).UTC()
	openID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	ago := func(d time.Duration) time.Time { return now.Add(-d) }

	cases := []struct {
		name        string
		s           agentSignals
		open, close bool
	}{
		{
			name: "healthy agent",
			s: agentSignals{
				LastSeen: ago(10 * time.Second), MinInterval: 10 * time.Second,
				LastResult: ago(5 * time.Second),
			},
		},
		{
			name: "results and stream both silent",
			s: agentSignals{
				LastSeen: ago(5 * time.Minute), MinInterval: 10 * time.Second,
				LastResult: ago(4 * time.Minute),
			},
			open: true,
		},
		{
			name: "stream silent but results flowing (server-side stream hiccup)",
			s: agentSignals{
				LastSeen: ago(5 * time.Minute), MinInterval: 10 * time.Second,
				LastResult: ago(5 * time.Second),
			},
		},
		{
			name: "results stale but stream alive (probes deleted or failing upstream)",
			s: agentSignals{
				LastSeen: ago(30 * time.Second), MinInterval: 10 * time.Second,
				LastResult: ago(10 * time.Minute),
			},
		},
		{
			name: "no probes configured, stream gone",
			s: agentSignals{
				LastSeen: ago(10 * time.Minute),
			},
			open: true,
		},
		{
			name: "never produced a result, stream gone",
			s: agentSignals{
				LastSeen: ago(10 * time.Minute), MinInterval: 10 * time.Second,
			},
			open: true,
		},
		{
			name: "already open, still silent: no second event",
			s: agentSignals{
				LastSeen: ago(10 * time.Minute), MinInterval: 10 * time.Second,
				OpenEventID: &openID,
			},
		},
		{
			name: "open event closes when the stream returns",
			s: agentSignals{
				LastSeen: ago(10 * time.Second), MinInterval: 10 * time.Second,
				LastResult: ago(10 * time.Minute), OpenEventID: &openID,
			},
			close: true,
		},
		{
			name: "open event closes when results return",
			s: agentSignals{
				LastSeen: ago(10 * time.Minute), MinInterval: 10 * time.Second,
				LastResult: ago(15 * time.Second), OpenEventID: &openID,
			},
			close: true,
		},
	}
	for _, c := range cases {
		open, closeEvent := decideOffline(now, c.s)
		if open != c.open || closeEvent != c.close {
			t.Errorf("%s: decideOffline = open=%v close=%v, want open=%v close=%v",
				c.name, open, closeEvent, c.open, c.close)
		}
	}
}
