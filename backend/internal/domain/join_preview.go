package domain

import (
	"encoding/json"
	"fmt"
	"sort"
)

var ErrJoinPreviewChanged = fmt.Errorf("%w: join preview changed; review the current plan before confirming", ErrConflict)

// JoinPolicyError is a stable business reason, independent of UI copy.
type JoinPolicyError struct {
	Code  string
	Cause error
}

func (e *JoinPolicyError) Error() string        { return e.Cause.Error() }
func (e *JoinPolicyError) Unwrap() error        { return e.Cause }
func joinDenied(code string, cause error) error { return &JoinPolicyError{Code: code, Cause: cause} }

// JoinScopeRevision binds approval to the published topology. Pending transcript
// growth and presentation metadata do not invalidate approval. Ref attachment,
// branch identity, natural ancestry and graft registers do. Stores recompute it
// under the same lock/transaction that applies the mutation.
func JoinScopeRevision(byID map[ContentHash]Snapshot, refs []Ref) ContentHash {
	type refState struct {
		Kind           RefKind
		Name, BranchID string
		Target         ContentHash
	}
	type nodeState struct {
		ID              ContentHash
		Parents, Grafts []ContentHash
		Seq             uint64
	}
	r := make([]refState, 0)
	roots := make([]ContentHash, 0)
	for _, ref := range refs {
		if ref.Kind != RefBranch && ref.Kind != RefSession {
			continue
		}
		r = append(r, refState{ref.Kind, ref.Name, ref.BranchID, ref.Target})
		roots = append(roots, ref.Target)
	}
	sort.Slice(r, func(i, j int) bool {
		if r[i].Kind != r[j].Kind {
			return r[i].Kind < r[j].Kind
		}
		return r[i].Name < r[j].Name
	})
	n := make([]nodeState, 0)
	for id := range snapshotReachableSet(byID, roots...) {
		s := byID[id]
		// Natural first-parent order is semantic. Graft order is a set.
		grafts := append([]ContentHash{}, s.GraftParents...)
		sort.Slice(grafts, func(i, j int) bool { return grafts[i] < grafts[j] })
		n = append(n, nodeState{id, append([]ContentHash{}, s.Parents...), grafts, s.GraftSeq})
	}
	sort.Slice(n, func(i, j int) bool { return n[i].ID < n[j].ID })
	raw, _ := json.Marshal(struct {
		Version int
		Refs    []refState
		Nodes   []nodeState
	}{1, r, n})
	return HashContent(raw)
}

// Revision excludes the allocated residual ref name: allocation occurs only
// after confirmation. It includes the full segment even for source-only joins.
func (p JoinPlan) Revision() ContentHash {
	p.Grafts = append([]GraftPatch{}, p.Grafts...)
	for i := range p.Grafts {
		p.Grafts[i].Parents = append([]ContentHash{}, p.Grafts[i].Parents...)
		sort.Slice(p.Grafts[i].Parents, func(a, b int) bool { return p.Grafts[i].Parents[a] < p.Grafts[i].Parents[b] })
	}
	sort.Slice(p.Grafts, func(i, j int) bool { return p.Grafts[i].SnapshotID < p.Grafts[j].SnapshotID })
	raw, _ := json.Marshal(p)
	return HashContent(raw)
}
