package store

// The needs_attention predicate is mirrored by the SPA's attentionReason
// (web/src/views/Overview.tsx and Agents.tsx) so the fleet card and the
// server-side Attention filter agree. The shared numbers live in
// testdata/attention-parity.json, read here and by
// web/test/attention-parity.test.mjs.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestNeedsAttentionWindowsMatchFixture(t *testing.T) {
	b, err := os.ReadFile("testdata/attention-parity.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var f struct {
		CertWarnDays       int `json:"cert_warn_days"`
		DropAttentionHours int `json:"drop_attention_hours"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}

	// The SPA flags floor(days-left) <= cert_warn_days; the SQL's absolute
	// window "not_after < now() + interval 'N days'" is equivalent only for
	// N = cert_warn_days + 1 (7.9 days left floors to 7 and must flag).
	certInterval := fmt.Sprintf("interval '%d days'", f.CertWarnDays+1)
	if !strings.Contains(agentInventoryCTE, certInterval) {
		t.Errorf("needs_attention lacks the cert window %q the SPA mirrors (cert_warn_days=%d)",
			certInterval, f.CertWarnDays)
	}
	dropInterval := fmt.Sprintf("interval '%d hours'", f.DropAttentionHours)
	if !strings.Contains(agentInventoryCTE, dropInterval) {
		t.Errorf("needs_attention lacks the drop window %q the SPA mirrors", dropInterval)
	}
}
