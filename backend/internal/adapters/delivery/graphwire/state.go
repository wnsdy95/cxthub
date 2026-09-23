// Package graphwire encodes repeated graph identifiers once per response.
// This is a transport representation, never a business rule or persisted graph.
package graphwire

import (
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"slices"
	"sort"
)

type State struct {
	domain.GraphState
	Encoding        string               `json:"encoding"`
	Dictionary      []domain.ContentHash `json:"dictionary"`
	SnapshotIDs     []uint32             `json:"snapshot_ids"`
	GraphIDs        []uint32             `json:"graph_ids"`
	CommittedIDs    []uint32             `json:"committed_ids"`
	HistoricalIDs   []uint32             `json:"historical_ids"`
	SharedIDs       []uint32             `json:"shared_ids"`
	PushedIDs       []uint32             `json:"pushed_ids"`
	UnpushedIDs     []uint32             `json:"unpushed_ids"`
	UncommittedIDs  []uint32             `json:"uncommitted_ids"`
	TaggedIDs       []uint32             `json:"tagged_ids"`
	ArchivedOnlyIDs []uint32             `json:"archived_only_ids"`
	AheadIDs        []uint32             `json:"ahead_ids"`
	AheadTips       []uint32             `json:"ahead_tips"`

	BranchSnapshots map[string][]uint32 `json:"branch_snapshots"`
	Hold            []Hold              `json:"hold"`
	Previous        []Progress          `json:"previous"`
}
type Hold struct {
	Tips []domain.Unsync `json:"tips"`
	IDs  []uint32        `json:"ids"`
}
type Progress struct {
	domain.GraphProgressGroup
	SnapshotIDs    []uint32 `json:"snapshot_ids"`
	CollapsibleIDs []uint32 `json:"collapsible_ids"`
}

func Encode(g domain.GraphState) State {
	out := State{GraphState: g, Encoding: "indexed-v1", Dictionary: []domain.ContentHash{}, BranchSnapshots: map[string][]uint32{}, Hold: []Hold{}, Previous: []Progress{}}
	indices := map[domain.ContentHash]uint32{}
	encode := func(ids []domain.ContentHash) []uint32 {
		result := make([]uint32, 0, len(ids))
		for _, id := range ids {
			index, ok := indices[id]
			if !ok {
				index = uint32(len(out.Dictionary))
				indices[id] = index
				out.Dictionary = append(out.Dictionary, id)
			}
			result = append(result, index)
		}
		return result
	}
	out.SnapshotIDs = encode(g.SnapshotIDs)
	out.GraphIDs = encode(g.GraphIDs)
	out.CommittedIDs = encode(g.CommittedIDs)
	out.HistoricalIDs = encode(g.HistoricalIDs)
	out.SharedIDs = encode(g.SharedIDs)
	out.PushedIDs = encode(g.PushedIDs)
	out.UnpushedIDs = encode(g.UnpushedIDs)
	out.UncommittedIDs = encode(g.UncommittedIDs)
	out.TaggedIDs = encode(g.TaggedIDs)
	out.ArchivedOnlyIDs = encode(g.ArchivedOnlyIDs)
	out.AheadIDs = encode(g.AheadIDs)
	out.AheadTips = encode(g.AheadTips)

	branches := make([]string, 0, len(g.BranchSnapshots))
	for branch := range g.BranchSnapshots {
		branches = append(branches, branch)
	}
	sort.Strings(branches)
	for _, branch := range branches {
		out.BranchSnapshots[branch] = encode(g.BranchSnapshots[branch])
	}
	for _, h := range g.Hold {
		out.Hold = append(out.Hold, Hold{Tips: h.Tips, IDs: encode(h.IDs)})
	}
	for _, p := range g.Previous {
		out.Previous = append(out.Previous, Progress{GraphProgressGroup: p, SnapshotIDs: encode(p.SnapshotIDs), CollapsibleIDs: encode(p.CollapsibleIDs)})
	}
	return out
}

// V2 reuses branch_snapshots rather than transmitting each timeline twice.
// V1 remains the default for clients that have not negotiated V2.
type StateV2 struct {
	State
	Encoding       string                   `json:"encoding"`
	BranchContexts map[string]BranchContext `json:"branch_contexts"`
}
type BranchContext struct {
	domain.BranchContext
	Roots          []uint32 `json:"roots"`
	SnapshotIDs    []uint32 `json:"snapshot_ids,omitempty"`
	SnapshotIDsRef string   `json:"snapshot_ids_ref,omitempty"`
}

func EncodeV2(g domain.GraphState) StateV2 {
	out := StateV2{State: Encode(g), Encoding: "indexed-v2", BranchContexts: map[string]BranchContext{}}
	indices := map[domain.ContentHash]uint32{}
	for i, h := range out.Dictionary {
		indices[h] = uint32(i)
	}
	encode := func(ids []domain.ContentHash) []uint32 {
		result := make([]uint32, 0, len(ids))
		for _, id := range ids {
			index, ok := indices[id]
			if !ok {
				index = uint32(len(out.Dictionary))
				indices[id] = index
				out.Dictionary = append(out.Dictionary, id)
			}
			result = append(result, index)
		}
		return result
	}
	branches := make([]string, 0, len(g.BranchContexts))
	for branch := range g.BranchContexts {
		branches = append(branches, branch)
	}
	sort.Strings(branches)
	for _, branch := range branches {
		c := g.BranchContexts[branch]
		wire := BranchContext{BranchContext: c, Roots: encode(c.Roots)}
		if ids, ok := g.BranchSnapshots[branch]; ok && slices.Equal(ids, c.SnapshotIDs) {
			wire.SnapshotIDsRef = branch
		} else {
			wire.SnapshotIDs = encode(c.SnapshotIDs)
		}
		out.BranchContexts[branch] = wire
	}
	return out
}
