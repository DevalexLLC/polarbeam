// Package meshexpand turns an agent's stored probe assignments — direct
// probes plus mesh templates — into the full ConfigSnapshot streamed to that
// agent. Pure: no database access, fully deterministic (identical inputs
// produce byte-identical snapshots and hashes regardless of input order).
package meshexpand

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	pb "github.com/devalexllc/polarbeam/internal/pb/polarbeamv1"
	"github.com/devalexllc/polarbeam/internal/server/probeid"
	"github.com/devalexllc/polarbeam/internal/server/store"
)

// BuildSnapshot expands one agent's config inputs into a snapshot.
//
// Direct probes keep their probe_configs.id as probe_id. Mesh templates
// expand over every peer agent — this agent is always the source — with
// probe_id = UUIDv5(template_config_id, "src_site|target"), so each
// (template, source site, destination agent) triple is its own series and
// the same expansion yields the same probe_id on every rebuild, keeping
// config hashes stable.
//
// Returns an error if the expansion derives the same probe_id twice —
// unreachable by construction (template rows and targets are DB-unique),
// but a duplicate would silently collapse probes in every ID-keyed consumer
// (agent scheduler, ingest allowlist, series state), so it fails loud.
func BuildSnapshot(in store.AgentConfigInputs) (*pb.ConfigSnapshot, error) {
	specs := make([]*pb.ProbeSpec, 0, len(in.Direct))

	for _, d := range in.Direct {
		kind := pb.TargetKind_TARGET_KIND_EXTERNAL
		if d.Kind == "agent" {
			kind = pb.TargetKind_TARGET_KIND_AGENT_PEER
		}
		specs = append(specs, spec(d.ID, d.Settings, &pb.Target{
			Kind:     kind,
			TargetId: d.TargetID.String(),
			Address:  d.Address,
			Port:     uint32(d.Port),
			Url:      d.URL,
		}))
	}

	for _, m := range in.Mesh {
		for _, p := range in.Peers {
			if p.MeshID != m.MeshID || p.SiteID == in.SiteID {
				continue
			}
			specs = append(specs, spec(probeid.MeshProbeID(m.ConfigID, in.SiteID, p.TargetID),
				m.Settings, &pb.Target{
					Kind:     pb.TargetKind_TARGET_KIND_AGENT_PEER,
					TargetId: p.TargetID.String(),
					Address:  p.ProbeAddress,
					Port:     meshPort(m.Settings.Params),
				}))
		}
	}

	sort.Slice(specs, func(i, j int) bool { return specs[i].ProbeId < specs[j].ProbeId })
	for i := 1; i < len(specs); i++ {
		if specs[i].ProbeId == specs[i-1].ProbeId {
			return nil, fmt.Errorf("duplicate probe_id %s in expansion for agent %s", specs[i].ProbeId, in.AgentID)
		}
	}
	return &pb.ConfigSnapshot{
		ConfigHash: hashSpecs(specs),
		Probes:     specs,
	}, nil
}

// meshPort reads the target port for mesh probes from the template's params
// ("port"). Mesh templates have no target row to carry one.
func meshPort(params map[string]string) uint32 {
	var port uint32
	fmt.Sscanf(params["port"], "%d", &port)
	return port
}

func spec(id uuid.UUID, ps store.ProbeSettings, target *pb.Target) *pb.ProbeSpec {
	s := &pb.ProbeSpec{
		ProbeId:  id.String(),
		Type:     pb.ProbeType(ps.ProbeType),
		Target:   target,
		Interval: durationpb.New(ps.Interval),
		Timeout:  durationpb.New(ps.Timeout),
	}
	if ps.TrainCount > 0 {
		s.TrainCount = uint32(ps.TrainCount)
		s.TrainSpacing = durationpb.New(ps.TrainSpacing)
	}
	if len(ps.Params) > 0 {
		s.Params = ps.Params
	}
	return s
}

// hashSpecs computes the snapshot's content hash: sha256 over the
// deterministic proto encoding of each sorted spec, length-prefixed so
// spec boundaries are unambiguous.
func hashSpecs(specs []*pb.ProbeSpec) string {
	h := sha256.New()
	opts := proto.MarshalOptions{Deterministic: true}
	var lenBuf [8]byte
	for _, s := range specs {
		b, err := opts.Marshal(s)
		if err != nil {
			// Marshal of a well-formed in-memory message cannot fail; if it
			// somehow does, poison the hash so the snapshot is re-sent rather
			// than wrongly considered unchanged.
			h.Write([]byte("marshal-error"))
			continue
		}
		binary.LittleEndian.PutUint64(lenBuf[:], uint64(len(b)))
		h.Write(lenBuf[:])
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}
