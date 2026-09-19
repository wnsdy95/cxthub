package domain

import (
	"fmt"
	"strings"
)

// JoinGraph is the complete repository view, never a folded or filtered UI graph.
// Memberships are projected from durable branch identity/history by the application.
type JoinGraph struct {
	Snapshots   []Snapshot
	Refs        []Ref
	Pendings    []Pending
	Memberships map[ContentHash]map[string]bool
}

type JoinRequest struct {
	RepoID             ContentHash
	Branch             string
	BranchID           string
	Source             ContentHash
	IncludeDescendants bool
}

// JoinPlan preserves natural parents and all previously reachable sessions.
// RemainingTip needs a scoped session-ref name before the plan becomes a mutation.
type JoinPlan struct {
	RepoID       ContentHash
	Branch       string
	BranchID     string
	Source       ContentHash
	Segment      []ContentHash
	ExpectedHead ContentHash
	NewHead      ContentHash
	RemainingTip ContentHash
	Grafts       []GraftPatch
}

func (p JoinPlan) Mutation(forkName string) (JoinMutation, error) {
	m := JoinMutation{RepoID: p.RepoID, Branch: p.Branch, BranchID: p.BranchID,
		Source: p.Source, Segment: p.Segment, ExpectedHead: p.ExpectedHead, NewHead: p.NewHead,
		ForkTip: p.RemainingTip, ForkName: forkName, Grafts: p.Grafts}
	if err := ValidateJoinMutation(m); err != nil {
		return JoinMutation{}, err
	}
	return m, nil
}

// PlanJoin decides eligibility and the exact graft change set without IO or mutation.
// Commands and read-only previews must both use this policy. Stores revalidate its
// invariants under their own write locks; a plan is not authorization to skip CAS.
func PlanJoin(graph JoinGraph, in JoinRequest) (JoinPlan, error) {
	for _, id := range []ContentHash{in.RepoID, in.Source} {
		if err := ValidateContentHash(id); err != nil {
			return JoinPlan{}, err
		}
	}
	if err := ValidateBranchName(in.Branch); err != nil {
		return JoinPlan{}, err
	}
	if in.Branch == HeadRefName {
		return JoinPlan{}, fmt.Errorf("%w: HEAD is not a joinable branch", ErrValidation)
	}
	snaps, refs, pendings, members := graph.Snapshots, graph.Refs, graph.Pendings, graph.Memberships
	byID := make(map[ContentHash]Snapshot, len(snaps))
	for _, snap := range snaps {
		if _, exists := byID[snap.ID]; exists {
			return JoinPlan{}, fmt.Errorf("%w: duplicate snapshot %s", ErrIntegrity, snap.ID)
		}
		if snap.RepoID != in.RepoID {
			return JoinPlan{}, fmt.Errorf("%w: snapshot outside repository", ErrIntegrity)
		}
		byID[snap.ID] = snap
	}
	// Only committed records participate. Unattached hook captures cannot be promoted through Join.
	// Conversely, past deduplication leaves hook labels, but if reachable from a branch/session/lifecycle root, it's already a shared commit and included.
	var sharedRoots, sessionRoots []ContentHash
	for _, ref := range refs {
		if joinSharedTimelineRef(ref) {
			sharedRoots = append(sharedRoots, ref.Target)
			if ref.Kind == RefSession && strings.HasPrefix(ref.Name, SessionRefPrefix(in.Branch)) {
				sessionRoots = append(sessionRoots, ref.Target)
			}
		}
	}
	shared := snapshotReachableSet(byID, sharedRoots...)
	targetSessionShared := snapshotReachableSet(byID, sessionRoots...)
	pendingTargets := map[ContentHash]bool{}
	for _, pending := range pendings {
		// A stale or dismissed pending pointer must not block an already shared commit.
		if pending.Dismissed || shared[pending.Target] {
			continue
		}
		pendingTargets[pending.Target] = true
	}
	isJoinCommit := func(id ContentHash) bool {
		snap, ok := byID[id]
		return ok && !pendingTargets[id] && (!strings.HasPrefix(snap.Message, HookMessagePrefix) || shared[id])
	}
	firstChildren := map[ContentHash][]ContentHash{}
	for _, sn := range snaps {
		if isJoinCommit(sn.ID) && len(sn.Parents) > 0 {
			firstChildren[sn.Parents[0]] = append(firstChildren[sn.Parents[0]], sn.ID)
		}
	}
	snapX, ok := byID[in.Source]
	if !ok {
		return JoinPlan{}, fmt.Errorf("%w: snapshot %s", ErrNotFound, in.Source)
	}
	if !isJoinCommit(in.Source) {
		return JoinPlan{}, fmt.Errorf("%w: pending hook capture cannot be joined", ErrValidation)
	}
	if !members[in.Source][in.Branch] {
		return JoinPlan{}, fmt.Errorf("%w: snapshot does not belong to git branch %q — cross-branch join is not allowed", ErrConflict, in.Branch)
	}
	var head Ref
	for _, ref := range refs {
		if ref.Kind == RefBranch && ref.Name == in.Branch {
			head = ref
			break
		}
	}
	if head.Target == "" {
		return JoinPlan{}, fmt.Errorf("%w: target branch %q has no head", ErrNotFound, in.Branch)
	}
	if head.Target == in.Source {
		return JoinPlan{}, fmt.Errorf("%w: snapshot is already the branch head", ErrConflict)
	}
	headReach := snapshotReachableSet(byID, head.Target)
	targetShared := make(map[ContentHash]bool, len(headReach)+len(targetSessionShared))
	for id := range headReach {
		targetShared[id] = true
	}
	for id := range targetSessionShared {
		targetShared[id] = true
	}
	// Join is an operation to reorder a shared session branch. It's not a bypass path for objects-only shadow push, which could elevate unpushed objects or past dangling snapshots to branch ref on web. Only allow in target branch's graft reach set or partial join session ref.
	if !targetShared[in.Source] {
		return JoinPlan{}, fmt.Errorf("%w: snapshot is not an attached session branch; push it first", ErrConflict)
	}
	// 1) Natural history determination: parents-only walk excluding grafts — branches connected only by grafts are reordering targets, natural history rejects (head retreat/circular source).
	if naturalReachable(byID, head.Target, in.Source) {
		return JoinPlan{}, fmt.Errorf("%w: snapshot already in branch %q natural history", ErrConflict, in.Branch)
	}
	// 2) Segments are not SessionID but natural first-parent path. Server extends from X to target git branch's unique child leaf. Multiple children mean unknown lane, so safely reject. Client doesn't specify move range/tip.
	segment := map[ContentHash]bool{}
	branchChildren := func(id ContentHash) []ContentHash {
		var out []ContentHash
		for _, kid := range firstChildren[id] {
			if members[kid][in.Branch] {
				out = append(out, kid)
			}
		}
		return out
	}
	tip := in.Source
	segment[tip] = true
	segmentIDs := []ContentHash{tip}
	seen := map[ContentHash]bool{tip: true}
	for {
		kids := branchChildren(tip)
		if len(kids) == 0 {
			break
		}
		if len(kids) > 1 {
			return JoinPlan{}, fmt.Errorf("%w: chain above snapshot forks — join a leaf after resolving the fork", ErrConflict)
		}
		tip = kids[0]
		// Objects-only shadow push existing natural descendants elevate to "full join" branch/session ref, bypassing cxt push public boundary. Opposite full join must be reachable from current branch or partial join session ref, i.e., pushed commits.
		if !targetShared[tip] {
			return JoinPlan{}, fmt.Errorf("%w: chain above snapshot contains an unpushed commit; push it first", ErrConflict)
		}
		if seen[tip] {
			return JoinPlan{}, fmt.Errorf("%w: cycle in natural lineage", ErrIntegrity)
		}
		seen[tip] = true
		segment[tip] = true
		segmentIDs = append(segmentIDs, tip)
	}
	newHead := in.Source
	if in.IncludeDescendants {
		newHead = tip
	}
	// 3) Supersede plan: Remove graft in-flow edge from outside to inside segment (auto-graft residue) to prevent reordering loop. Segment reachability continues with new head/session ref, so total reach set doesn't shrink.
	type patch struct {
		id   ContentHash
		next []ContentHash
	}
	var supersedes []patch
	// graft register is snapshot global meta, if another git branch ref reaches the same source, edge removal changes the branch graph. Shared sources cannot be safely superseded without branch-scoped placement, so it is rejected.
	var otherRoots []ContentHash
	targetSessionPrefix := SessionRefPrefix(in.Branch)
	for _, ref := range refs {
		otherScope := (ref.Kind == RefBranch && ref.Name != in.Branch) ||
			(ref.Kind == RefSession && !strings.HasPrefix(ref.Name, targetSessionPrefix))
		if otherScope && ref.Target != "" {
			otherRoots = append(otherRoots, ref.Target)
		}
	}
	otherBranchReach := snapshotReachableSet(byID, otherRoots...)
	for _, id := range segmentIDs {
		if otherBranchReach[id] {
			return JoinPlan{}, fmt.Errorf("%w: snapshot %s is currently reachable from another git branch", ErrConflict, id)
		}
	}
	for _, sn := range snaps {
		if segment[sn.ID] || len(sn.GraftParents) == 0 || !headReach[sn.ID] {
			continue
		}
		var next []ContentHash
		hit := false
		for _, g := range sn.GraftParents {
			if segment[g] {
				hit = true
				continue
			}
			next = append(next, g)
		}
		if hit {
			if !members[sn.ID][in.Branch] {
				return JoinPlan{}, fmt.Errorf("%w: a graft from another git branch blocks this join", ErrConflict)
			}
			if otherBranchReach[sn.ID] {
				return JoinPlan{}, fmt.Errorf("%w: graft source %s is shared by another git branch", ErrConflict, sn.ID)
			}
			supersedes = append(supersedes, patch{id: sn.ID, next: next})
		}
	}
	patches := make([]GraftPatch, 0, len(supersedes)+1)
	for _, p := range supersedes {
		patches = append(patches, GraftPatch{SnapshotID: p.id, ExpectedSeq: byID[p.id].GraftSeq, Parents: p.next})
	}
	xNext := append([]ContentHash{}, snapX.GraftParents...)
	foundHead := false
	for _, parent := range xNext {
		foundHead = foundHead || parent == head.Target
	}
	if !foundHead {
		xNext = append(xNext, head.Target)
	}
	patches = append(patches, GraftPatch{SnapshotID: in.Source, ExpectedSeq: snapX.GraftSeq, Parents: xNext})

	remaining := ContentHash("")
	if !in.IncludeDescendants && tip != in.Source {
		remaining = tip
	}
	return JoinPlan{RepoID: in.RepoID, Branch: in.Branch, BranchID: in.BranchID,
		Source: in.Source, Segment: segmentIDs, ExpectedHead: head.Target, NewHead: newHead,
		RemainingTip: remaining, Grafts: patches}, nil
}

func joinSharedTimelineRef(ref Ref) bool {
	if ref.Target == "" {
		return false
	}
	if ref.Kind == RefBranch || ref.Kind == RefSession {
		return true
	}
	_, lifecycle, err := ParseBranchLifecycleRef(ref)
	return err == nil && lifecycle
}

func snapshotReachableSet(byID map[ContentHash]Snapshot, heads ...ContentHash) map[ContentHash]bool {
	out := map[ContentHash]bool{}
	stack := append([]ContentHash{}, heads...)
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == "" || out[cur] {
			continue
		}
		out[cur] = true
		if snap, ok := byID[cur]; ok {
			stack = append(stack, snap.ReachabilityParents()...)
		}
	}
	return out
}

// naturalReachable determines if from follows natural parents to anc (excluding grafts).
func naturalReachable(byID map[ContentHash]Snapshot, from, anc ContentHash) bool {
	if from == anc {
		return true
	}
	seen := map[ContentHash]bool{}
	stack := []ContentHash{from}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == anc {
			return true
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		stack = append(stack, byID[cur].Parents...)
	}
	return false
}
