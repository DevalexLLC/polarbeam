// Package probeid derives the stable probe IDs shared by config expansion
// (meshexpand) and admin mutations (store cleanup of expanded series). It is
// a leaf package — meshexpand imports store, so store cannot reach the
// derivation through meshexpand without a cycle.
package probeid

import (
	"fmt"

	"github.com/google/uuid"
)

// MeshProbeID is UUIDv5(template, "src_site|target"): stable across
// rebuilds, distinct per template row (so duplicate same-type templates are
// distinct series), per source site, and per destination agent (the target
// row is the peer agent's unique agent-kind target). A→B ≠ B→A because the
// targets differ. The source stays at site granularity so member-removal
// cleanup can derive one site's outbound series without touching other
// sources'; per-agent disambiguation on the source side comes from the
// agent_id column of every keyed table, exactly as for direct probes.
//
// The derivation is frozen — these IDs are stored in probe_results, so any
// change orphans mesh history. A cautionary precedent: an earlier
// UUIDv5(mesh, "src|dst|type") derivation omitted the destination agent and
// the template row, collapsing distinct probes onto one ID, and replacing
// it required a state-cleanup migration.
func MeshProbeID(templateID, srcSiteID, targetID uuid.UUID) uuid.UUID {
	name := fmt.Sprintf("%s|%s", srcSiteID, targetID)
	return uuid.NewSHA1(templateID, []byte(name))
}
