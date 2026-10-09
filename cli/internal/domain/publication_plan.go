package domain

import (
	"container/heap"
	"fmt"
	"reflect"
	"slices"
	"sort"
)

type PublicationBranch struct{ Branch, BranchID string }
type PublicationScope struct {
	Branches    []PublicationBranch
	HistoryOnly bool
	// Deferred Git pushes authorize the completed capture, never a newer local
	// context tip. Empty preserves the interactive publication contract.
	ExpectedTargets map[string]ContentHash
}

// PlanPublication is only for selected publication. The existing unselected
// manual push does not call it; an empty selection never means all branches.
type PublicationPlanInput struct {
	RepoID            string
	ContextProtocol   int
	Ref               string // canonical context name, normalized to the same identity scope
	Scope             PublicationScope
	Refs              []Ref
	History, Accepted []HistoryEvent
	Snapshots         []Snapshot // frozen graph for completion-group reachability proofs
}

type PublicationPlan struct {
	Authority     []PublicationBranch
	HistoryToSend []HistoryEvent // exact payload membership authorizes ordinary witnesses
	RefsToPush    []Ref
	SnapshotRoots []ContentHash // dependencies, not certification of upload readiness
}

type PublicationBlockedError struct{ EventID, DependencyID, BranchID, Reason string }

func (e *PublicationBlockedError) Error() string {
	return fmt.Sprintf("publication %s requires %s (%s): %s", e.EventID, e.DependencyID, e.BranchID, e.Reason)
}
func (e *PublicationBlockedError) Unwrap() error { return ErrSyncConflict }

func PlanPublication(in PublicationPlanInput) (PublicationPlan, error) {
	var out PublicationPlan
	if in.ContextProtocol != 1 {
		return PublicationPlan{}, ErrContextProtocolRequired
	}
	if err := ValidateContentHash(ContentHash(in.RepoID)); err != nil {
		return PublicationPlan{}, err
	}
	if in.Ref != "" && (len(in.Scope.Branches) != 0 || in.Scope.HistoryOnly) {
		return PublicationPlan{}, ErrInvalidRef
	}
	// Normalize input order, not payload timestamps: explicit edges govern causality.
	events := append([]HistoryEvent(nil), in.History...)
	sort.Slice(events, func(i, j int) bool {
		if events[i].CreatedAt.Equal(events[j].CreatedAt) {
			return events[i].ID < events[j].ID
		}
		return events[i].CreatedAt.Before(events[j].CreatedAt)
	})
	catalog, err := indexPublicationHistory(in.RepoID, events, in.Accepted)
	if err != nil {
		return PublicationPlan{}, err
	}
	// Accepted evidence can reject local authority, never select a new identity.
	localIdentity := func(ref Ref) (string, error) {
		if ref.BranchID != "" {
			return ref.BranchID, nil
		}
		local, err := projectPublicationBranches(events, []PublicationBranch{{Branch: ref.Name}})
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrSyncConflict, err)
		}
		return local.Identity(in.RepoID, ref.Name), nil
	}
	refs := map[string]Ref{}
	for _, r := range in.Refs {
		if r.Kind != RefBranch {
			continue
		}
		if r.RepoID != in.RepoID || ValidateRef(r) != nil {
			return PublicationPlan{}, ErrHashMismatch
		}
		if old, ok := refs[r.Name]; ok && old != r {
			return PublicationPlan{}, ErrHashMismatch
		}
		refs[r.Name] = r
	}
	branches := append([]PublicationBranch(nil), in.Scope.Branches...)
	if in.Ref != "" {
		if err := ValidateBranchName(in.Ref); err != nil {
			return PublicationPlan{}, err
		}
		ref, ok := refs[in.Ref]
		if !ok {
			return PublicationPlan{}, ErrNotFound
		}
		id, err := localIdentity(ref)
		if err != nil {
			return PublicationPlan{}, err
		}
		branches = []PublicationBranch{{in.Ref, id}}
	}
	if len(branches) == 0 {
		return PublicationPlan{}, ErrInvalidRef
	}
	if len(in.Scope.ExpectedTargets) > 0 {
		if in.Scope.HistoryOnly || len(in.Scope.ExpectedTargets) != len(branches) {
			return PublicationPlan{}, ErrInvalidRef
		}
		for _, b := range branches {
			target := in.Scope.ExpectedTargets[b.BranchID]
			if ValidateContentHash(target) != nil || refs[b.Branch].Target != target {
				return PublicationPlan{}, ErrSyncConflict
			}
		}
	}
	projection, err := projectPublicationBranches(catalog.all, branches)
	if err != nil {
		return PublicationPlan{}, fmt.Errorf("%w: %v", ErrSyncConflict, err)
	}
	allowed := map[string]bool{}
	names := map[string]string{}
	for _, b := range branches {
		// A history-only scope has no target. Use the already-validated repository
		// hash solely to reuse Ref envelope validation for its name and identity.
		shape := Ref{Kind: RefBranch, Name: b.Branch, BranchID: b.BranchID, Target: ContentHash(in.RepoID)}
		if b.BranchID == "" || ValidateRef(shape) != nil {
			return PublicationPlan{}, ErrInvalidRef
		}
		if name, ok := names[b.BranchID]; ok {
			if name != b.Branch {
				return PublicationPlan{}, ErrSyncConflict
			}
			continue
		}
		known, exists := projection.ByID[b.BranchID]
		if exists {
			if known.Name != b.Branch || (!in.Scope.HistoryOnly && known.Archived) {
				return PublicationPlan{}, ErrSyncConflict
			}
		} else if b.BranchID != LegacyContextBranchID(in.RepoID, b.Branch) {
			return PublicationPlan{}, ErrSyncConflict
		}
		if !in.Scope.HistoryOnly {
			r, ok := refs[b.Branch]
			if !ok {
				return PublicationPlan{}, ErrNotFound
			}
			localID, err := localIdentity(r)
			if err != nil {
				return PublicationPlan{}, err
			}
			if projection.Identity(in.RepoID, b.Branch) != b.BranchID || localID != b.BranchID {
				return PublicationPlan{}, ErrSyncConflict
			}
			if r.Symbolic != "" || r.Target == "" || ValidateContentHash(r.Target) != nil {
				return PublicationPlan{}, ErrInvalidRef
			}
			r.BranchID = b.BranchID
			out.RefsToPush = append(out.RefsToPush, r)
		}
		names[b.BranchID], allowed[b.BranchID] = b.Branch, true
		out.Authority = append(out.Authority, b)
	}
	sort.Slice(out.Authority, func(i, j int) bool { return out.Authority[i].BranchID < out.Authority[j].BranchID })
	sort.Slice(out.RefsToPush, func(i, j int) bool { return out.RefsToPush[i].Name < out.RefsToPush[j].Name })
	ancestry, err := publicationAncestry(in.RepoID, in.Snapshots, catalog.ancestryRoots(allowed))
	if err != nil {
		return PublicationPlan{}, err
	}
	ordered, err := catalog.order(in.RepoID, allowed, ancestry)
	if err != nil {
		return PublicationPlan{}, err
	}

	send := map[string]HistoryEvent{}
	edges := map[string]map[string]bool{}
	visiting := map[string]bool{}
	var visit func(string) error
	visit = func(id string) error {
		if catalog.accepted[id] {
			return nil
		}
		if _, ok := send[id]; ok {
			return nil
		}
		e, ok := catalog.byID[id]
		if !ok {
			return &PublicationBlockedError{DependencyID: id, Reason: "missing history dependency"}
		}
		if visiting[id] {
			return &PublicationBlockedError{EventID: id, Reason: "cyclic publication dependency"}
		}
		if e.Kind == "pr-merge" || (!allowed[e.BranchID] && e.Kind != "position" && e.Kind != "attach") {
			return &PublicationBlockedError{DependencyID: id, BranchID: e.BranchID, Reason: "unaccepted event would exceed publication authority"}
		}
		visiting[id] = true
		defer delete(visiting, id)
		deps := map[string]bool{}
		need := func(parent string) error {
			if parent == "" {
				return nil
			}
			if err := visit(parent); err != nil {
				return fmt.Errorf("event %s: %w", id, err)
			}
			deps[parent] = true
			return nil
		}
		for _, parent := range HistoryDependencies(e) {
			if err := need(parent); err != nil {
				return err
			}
		}
		if e.Creation != nil && e.Creation.OriginBranchID == e.BranchID && e.Kind != "attach" {
			return &PublicationBlockedError{EventID: id, Reason: "branch cannot be its own creation origin"}
		}
		if origin := e.Creation; origin != nil && origin.OriginBranchID != "" && origin.OriginBranchID != LegacyContextBranchID(e.RepoID, origin.OriginBranch) {
			candidates := []HistoryEvent{}
			for _, witness := range catalog.all {
				if witness.Kind == "publish" && !catalog.accepted[witness.ID] {
					continue
				}
				if witness.ID != id && witness.BranchID == origin.OriginBranchID && (witness.Branch == origin.OriginBranch || (witness.Kind == "rename" && witness.PreviousBranch == origin.OriginBranch)) {
					candidates = append(candidates, witness)
				}
			}
			// Prefer an accepted witness; then ordinary evidence; IDs only choose
			// between equivalent candidates, never establish a causal ordering.
			sort.Slice(candidates, func(i, j int) bool {
				a, b := candidates[i], candidates[j]
				if catalog.accepted[a.ID] != catalog.accepted[b.ID] {
					return catalog.accepted[a.ID]
				}
				ao, bo := a.Kind == "position" || a.Kind == "attach", b.Kind == "position" || b.Kind == "attach"
				if ao != bo {
					return ao
				}
				return a.ID < b.ID
			})
			var cause error
			found := false
			for _, witness := range candidates {
				before := map[string]bool{}
				for key := range send {
					before[key] = true
				}
				cause = need(witness.ID)
				if cause == nil {
					found = true
					break
				}
				// A rejected optional witness must not leave its partial closure authorized.
				for key := range send {
					if !before[key] {
						delete(send, key)
						delete(edges, key)
					}
				}
			}
			if !found {
				if cause != nil {
					return cause
				}
				return &PublicationBlockedError{EventID: id, BranchID: origin.OriginBranchID, Reason: "creation origin has no usable witness"}
			}
		}
		if e.Kind == "publish" {
			proven := false
			for _, proof := range catalog.all {
				if !publicationProof(e, proof) {
					continue
				}
				proven = true
				if err := need(proof.ID); err != nil {
					return err
				}
				if e.LocalBranch != "" && e.LocalBranch != e.Branch && e.WorktreeID != "" {
					for _, a := range catalog.all {
						if publicationAliasAttachment(e, proof, a) {
							if err := need(a.ID); err != nil {
								return err
							}
						}
					}
				}
			}
			if !proven {
				return &PublicationBlockedError{EventID: id, Reason: "publication has no exact ordinary proof"}
			}
		}
		send[id], edges[id] = clonePublicationEvent(e), deps
		return nil
	}
	for _, e := range ordered {
		if err := visit(e.ID); err != nil {
			return PublicationPlan{}, err
		}
	}
	// Foreign witnesses precede all completion barriers. Group order remains the
	// extracted maximal-source policy, even when clocks or input order differ.
	ordinary := []HistoryEvent{}
	publications := []HistoryEvent{}
	for _, e := range send {
		if e.Kind != "publish" {
			ordinary = append(ordinary, e)
		}
	}
	sort.Slice(ordinary, func(i, j int) bool {
		if ordinary[i].CreatedAt.Equal(ordinary[j].CreatedAt) {
			return ordinary[i].ID < ordinary[j].ID
		}
		return ordinary[i].CreatedAt.Before(ordinary[j].CreatedAt)
	})
	prior := ""
	for _, e := range ordered {
		if e.Kind == "publish" {
			publications = append(publications, send[e.ID])
			if prior != "" {
				edges[e.ID][prior] = true
			}
			prior = e.ID
		}
	}
	out.HistoryToSend, err = orderPublicationDependencies(append(ordinary, publications...), edges, len(ordinary))
	if err != nil {
		return PublicationPlan{}, err
	}
	roots := map[ContentHash]bool{}
	for _, r := range out.RefsToPush {
		roots[r.Target] = true
	}
	for _, e := range out.HistoryToSend {
		for _, id := range []ContentHash{e.Source, e.Target, e.SharedTarget, e.MemorySource} {
			if id != "" {
				roots[id] = true
			}
		}
	}
	for id := range roots {
		out.SnapshotRoots = append(out.SnapshotRoots, id)
	}
	sort.Slice(out.SnapshotRoots, func(i, j int) bool { return out.SnapshotRoots[i] < out.SnapshotRoots[j] })
	return out, nil
}

// This only verifies the graph needed to choose maximal completion sources.
// Body/attachment verification and mutable-queue execution belong to the executor.
func publicationAncestry(repo string, snapshots []Snapshot, roots []ContentHash) (map[ContentHash]map[ContentHash]bool, error) {
	byID := map[ContentHash]Snapshot{}
	for _, s := range snapshots {
		if old, ok := byID[s.ID]; ok && !reflect.DeepEqual(old, s) {
			return nil, ErrHashMismatch
		}
		byID[s.ID] = s
	}
	out := map[ContentHash]map[ContentHash]bool{}
	for _, root := range roots {
		if out[root] != nil {
			continue
		}
		seen, active := map[ContentHash]bool{}, map[ContentHash]bool{}
		var walk func(ContentHash) error
		walk = func(id ContentHash) error {
			if active[id] {
				return fmt.Errorf("%w: cyclic publication graph", ErrSyncConflict)
			}
			if seen[id] {
				return nil
			}
			s, ok := byID[id]
			if !ok {
				return fmt.Errorf("%w: missing publication snapshot %s", ErrNotFound, id)
			}
			if s.ID != id || s.RepoID != repo || ValidateContentHash(id) != nil {
				return ErrHashMismatch
			}
			active[id] = true
			for _, p := range s.ReachabilityParents() {
				if err := walk(p); err != nil {
					return err
				}
			}
			delete(active, id)
			seen[id] = true
			return nil
		}
		if err := walk(root); err != nil {
			return nil, err
		}
		out[root] = seen
	}
	return out, nil
}

func clonePublicationEvent(e HistoryEvent) HistoryEvent {
	if e.Creation != nil {
		c := *e.Creation
		c.Command = slices.Clone(c.Command)
		e.Creation = &c
	}
	if e.PR != nil {
		p := *e.PR
		e.PR = &p
	}
	return e
}

func orderPublicationDependencies(events []HistoryEvent, edges map[string]map[string]bool, ordinary int) ([]HistoryEvent, error) {
	index := map[string]int{}
	for i, e := range events {
		index[e.ID] = i
	}
	degree := make([]int, len(events))
	children := make([][]int, len(events))
	q := &publicationOrder{}
	for i, e := range events {
		for parent := range edges[e.ID] {
			if j, ok := index[parent]; ok {
				degree[i]++
				children[j] = append(children[j], i)
			}
		}
		if degree[i] == 0 {
			heap.Push(q, i)
		}
	}
	out := make([]HistoryEvent, 0, len(events))
	for q.Len() > 0 {
		i := heap.Pop(q).(int)
		if i >= ordinary && len(out) < ordinary {
			return nil, fmt.Errorf("%w: completion precedes ordinary dependency", ErrSyncConflict)
		}
		out = append(out, events[i])
		for _, child := range children[i] {
			degree[child]--
			if degree[child] == 0 {
				heap.Push(q, child)
			}
		}
	}
	if len(out) != len(events) {
		return nil, fmt.Errorf("%w: cyclic publication dependency", ErrSyncConflict)
	}
	return out, nil
}

type publicationOrder []int

func (q publicationOrder) Len() int           { return len(q) }
func (q publicationOrder) Less(i, j int) bool { return q[i] < q[j] }
func (q publicationOrder) Swap(i, j int)      { q[i], q[j] = q[j], q[i] }
func (q *publicationOrder) Push(x any)        { *q = append(*q, x.(int)) }
func (q *publicationOrder) Pop() any          { a := *q; n := len(a) - 1; *q = a[:n]; return a[n] }
