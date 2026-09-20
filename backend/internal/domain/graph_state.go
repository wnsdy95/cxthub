package domain

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// GraphState is an identifier-only business projection, not stored graph edges.
// Consumers own selection/folding/coordinates, never publication or identity.
type GraphState struct {
	Integrations    []GraphIntegration         `json:"integrations"`
	BranchContexts  map[string]BranchContext   `json:"branch_contexts"`
	BranchSnapshots map[string][]ContentHash   `json:"branch_snapshots"`
	Continuations   map[ContentHash]string     `json:"continuations"`
	Operations      GraphOperations            `json:"operations"`
	Version         int                        `json:"version"`
	Revision        RepositoryRevision         `json:"revision"`
	PositionEvent   string                     `json:"position_event,omitempty"`
	PrimaryBranch   string                     `json:"primary_branch"`
	SnapshotIDs     []ContentHash              `json:"snapshot_ids"`
	GraphIDs        []ContentHash              `json:"graph_ids"`
	CommittedIDs    []ContentHash              `json:"committed_ids"`
	HistoricalIDs   []ContentHash              `json:"historical_ids"`
	SharedIDs       []ContentHash              `json:"shared_ids"`
	PushedIDs       []ContentHash              `json:"pushed_ids"`
	UnpushedIDs     []ContentHash              `json:"unpushed_ids"`
	UncommittedIDs  []ContentHash              `json:"uncommitted_ids"`
	TaggedIDs       []ContentHash              `json:"tagged_ids"`
	ArchivedOnlyIDs []ContentHash              `json:"archived_only_ids"`
	AheadIDs        []ContentHash              `json:"ahead_ids"`
	AheadTips       []ContentHash              `json:"ahead_tips"`
	Markers         []GraphBranchMarker        `json:"markers"`
	BranchHeads     map[string]GraphBranchHead `json:"branch_heads"`
	RefScopes       map[string]string          `json:"ref_scopes"`
	SnapshotScopes  map[string]string          `json:"snapshot_scopes"`
	ScopeLabels     map[string]string          `json:"scope_labels"`
	Hold            []GraphHoldCluster         `json:"hold"`
	OrphanSessions  []string                   `json:"orphan_sessions"`
	HoldCounts      map[string]int             `json:"hold_counts"`
	Positions       []GraphPosition            `json:"positions"`
	Previous        []GraphProgressGroup       `json:"previous"`
}
type GraphBranchHead struct {
	Branch   string `json:"branch"`
	EventID  string `json:"event_id"`
	Archived bool   `json:"archived"`
}
type GraphBranchMarker struct {
	Branch          string      `json:"branch"`
	Target          ContentHash `json:"target"`
	Kind            string      `json:"kind"`
	UniqueCount     int         `json:"unique_count"`
	TargetAvailable bool        `json:"target_available"`
}
type GraphHoldCluster struct {
	Tips []Unsync      `json:"tips"`
	IDs  []ContentHash `json:"ids"`
}
type GraphPosition struct {
	EventID   string      `json:"event_id"`
	Branch    string      `json:"branch"`
	BranchID  string      `json:"branch_id"`
	Snapshot  ContentHash `json:"snapshot"`
	Archived  bool        `json:"archived"`
	CreatedAt time.Time   `json:"created_at"`
}
type GraphProgressGroup struct {
	Key            string        `json:"key"`
	Branch         string        `json:"branch"`
	Before         ContentHash   `json:"before"`
	After          ContentHash   `json:"after"`
	CreatedAt      time.Time     `json:"created_at"`
	SnapshotIDs    []ContentHash `json:"snapshot_ids"`
	CollapsibleIDs []ContentHash `json:"collapsible_ids"`
}
type graphSet map[ContentHash]bool

func graphIDs(s graphSet) []ContentHash {
	out := make([]ContentHash, 0, len(s))
	for id, yes := range s {
		if yes {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
func graphUnion(into graphSet, from map[ContentHash]bool) {
	for id := range from {
		into[id] = true
	}
}
func graphSubset(ids graphSet, keep func(ContentHash) bool) graphSet {
	out := graphSet{}
	for id := range ids {
		if keep(id) {
			out[id] = true
		}
	}
	return out
}

// A static, reusable metadata index. It never interprets labels as evidence of
// publication. Missing ancestors remain incomplete; cycles/duplicates fail.
type graphStateIndex struct {
	byID          map[ContentHash]Snapshot
	closure       map[ContentHash]graphClosure
	closureWeight int
	ordinal       map[ContentHash]int
	nodeIDs       []ContentHash
	parents       [][]int
	missing       []bool
}
type graphClosure struct {
	ids      graphClosureIDs
	complete bool
}

func newGraphStateIndex(snaps []Snapshot) (*graphStateIndex, error) {
	x := &graphStateIndex{byID: map[ContentHash]Snapshot{}, closure: map[ContentHash]graphClosure{}}
	for _, s := range snaps {
		if _, ok := x.byID[s.ID]; ok {
			return nil, fmt.Errorf("%w: duplicate graph snapshot %s", ErrIntegrity, s.ID)
		}
		x.byID[s.ID] = s
	}
	degrees := map[ContentHash]int{}
	children := map[ContentHash][]ContentHash{}
	ready := []ContentHash{}
	for id, s := range x.byID {
		for _, p := range s.ReachabilityParents() {
			if _, ok := x.byID[p]; ok {
				degrees[id]++
				children[p] = append(children[p], id)
			}
		}
		if degrees[id] == 0 {
			ready = append(ready, id)
		}
	}
	n := 0
	for len(ready) > 0 {
		id := ready[len(ready)-1]
		ready = ready[:len(ready)-1]
		n++
		for _, c := range children[id] {
			degrees[c]--
			if degrees[c] == 0 {
				ready = append(ready, c)
			}
		}
	}
	if n != len(x.byID) {
		return nil, fmt.Errorf("%w: cyclic graph ancestry", ErrIntegrity)
	}
	x.ordinal = make(map[ContentHash]int, len(snaps))
	x.nodeIDs = make([]ContentHash, 0, len(snaps))
	for id := range x.byID {
		x.nodeIDs = append(x.nodeIDs, id)
	}
	sort.Slice(x.nodeIDs, func(i, j int) bool { return x.nodeIDs[i] < x.nodeIDs[j] })
	for i, id := range x.nodeIDs {
		x.ordinal[id] = i
	}
	x.parents = make([][]int, len(snaps))
	x.missing = make([]bool, len(snaps))
	for i, id := range x.nodeIDs {
		for _, p := range x.byID[id].ReachabilityParents() {
			if n, ok := x.ordinal[p]; ok {
				x.parents[i] = append(x.parents[i], n)
			} else {
				x.missing[i] = true
			}
		}
	}
	return x, nil
}
func (x *graphStateIndex) from(root ContentHash) graphClosure {
	if c, ok := x.closure[root]; ok {
		return c
	}
	c := graphClosure{ids: graphClosureIDs{index: x}, complete: true}
	first, ok := x.ordinal[root]
	if !ok {
		c.complete = false
		return c
	}
	c.ids.words = make([]uint64, (len(x.nodeIDs)+63)/64)
	stack := []int{first}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		mask := uint64(1) << uint(n%64)
		if c.ids.words[n/64]&mask != 0 {
			continue
		}
		c.ids.words[n/64] |= mask
		if x.missing[n] {
			c.complete = false
		}
		stack = append(stack, x.parents[n]...)
	}
	// Per-read bitsets reuse ancestry without allocating a hash map for each
	// branch/merge. A hard byte/entry budget bounds memory even on wide DAGs.
	weight := len(c.ids.words) * 8
	if len(x.closure) < 4096 && x.closureWeight+weight <= 8<<20 {
		x.closure[root] = c
		x.closureWeight += weight
	}
	return c
}
func (x *graphStateIndex) roots(roots []ContentHash) graphSet {
	out := ContextClosure(x.byID, roots)
	for id := range out {
		if _, ok := x.byID[id]; !ok {
			delete(out, id)
		}
	}
	return out
}

func ProjectGraphState(v RepositoryView, primary, positionID string) (GraphState, error) {
	out := GraphState{BranchContexts: map[string]BranchContext{}, Version: 1, Revision: v.Revision, PositionEvent: positionID, Markers: []GraphBranchMarker{}, BranchHeads: map[string]GraphBranchHead{}, RefScopes: map[string]string{}, SnapshotScopes: map[string]string{}, ScopeLabels: map[string]string{}, Hold: []GraphHoldCluster{}, OrphanSessions: []string{}, HoldCounts: map[string]int{}, Positions: []GraphPosition{}, Previous: []GraphProgressGroup{}}
	x, err := newGraphStateIndex(v.Snapshots)
	if err != nil {
		return out, err
	}
	bindings, err := ProjectContextBranches(v.History)
	if err != nil {
		return out, fmt.Errorf("%w: %v", ErrIntegrity, err)
	}
	for id, b := range bindings.ByID {
		out.BranchHeads[id] = GraphBranchHead{Branch: b.Name, EventID: b.EventID, Archived: b.Archived}
		out.ScopeLabels[id] = b.Name
	}
	claims := map[string]map[string]bool{}
	for _, e := range v.History {
		if claims[e.Branch] == nil {
			claims[e.Branch] = map[string]bool{}
		}
		claims[e.Branch][e.BranchID] = true
	}
	legacyKey := func(name string) string {
		if len(claims[name]) == 1 {
			for id := range claims[name] {
				return id
			}
		}
		return "legacy:" + name
	}
	scope := func(name, identity string) string {
		if identity != "" {
			return identity
		}
		if b, ok := bindings.Active[name]; ok {
			return b.ID
		}
		return legacyKey(name)
	}
	var sharedRoots, tagRoots, activeRoots []ContentHash
	branches := map[string]Ref{}
	for _, r := range v.Refs {
		_, lifecycle, e := ParseBranchLifecycleRef(r)
		if e != nil {
			return out, e
		}
		if r.Kind == RefBranch {
			branches[r.Name] = r
			key := scope(r.Name, r.BranchID)
			out.RefScopes[r.Name] = key
			if out.ScopeLabels[key] == "" {
				out.ScopeLabels[key] = r.Name
			}
		}
		if r.Kind == RefBranch || r.Kind == RefSession || lifecycle {
			sharedRoots = append(sharedRoots, r.Target)
		}
		if r.Kind == RefTag && !lifecycle {
			tagRoots = append(tagRoots, r.Target)
		}
		if r.Kind == RefBranch || r.Kind == RefSession || (r.Kind == RefTag && !lifecycle) {
			activeRoots = append(activeRoots, r.Target)
		}
	}
	for _, s := range v.Snapshots {
		if len(claims[s.Branch]) > 1 {
			continue
		}
		key := legacyKey(s.Branch)
		if len(claims[s.Branch]) == 0 && branches[s.Branch].BranchID != "" {
			key = branches[s.Branch].BranchID
		}
		out.SnapshotScopes[s.Branch] = key
	}
	if _, ok := branches[primary]; !ok {
		primary = ""
		for _, candidate := range []string{"main", "master"} {
			if _, ok := branches[candidate]; ok {
				primary = candidate
				break
			}
		}
		if primary == "" {
			names := []string{}
			for name := range branches {
				names = append(names, name)
			}
			sort.Strings(names)
			if len(names) > 0 {
				primary = names[0]
			}
		}
	}
	out.PrimaryBranch = primary
	historicalRoots := []ContentHash{}
	for _, e := range v.Reflog {
		if e.Kind == RefBranch {
			historicalRoots = append(historicalRoots, e.Old, e.New)
		}
	}
	for _, e := range v.History {
		switch e.Kind {
		case "advance":
			historicalRoots = append(historicalRoots, e.Source, e.Target)
		case "publish":
			historicalRoots = append(historicalRoots, e.Target)
		case "pr-merge":
			if e.PRCompleted {
				historicalRoots = append(historicalRoots, e.Source, e.Target, e.SharedTarget)
			}
		}
	}
	historical := x.roots(historicalRoots)
	shared := x.roots(sharedRoots)
	graphUnion(shared, historical)
	kept := graphSet{}
	for _, s := range v.Snapshots {
		if s.Branch != "(stash)" || shared[s.ID] {
			kept[s.ID] = true
		}
	}
	hold, inHold := graphHold(v.Unsync, x, shared, kept)
	out.Hold = hold
	pendings := append([]Pending(nil), v.Pending...)
	sort.Slice(pendings, func(i, j int) bool {
		if pendings[i].UpdatedAt.Equal(pendings[j].UpdatedAt) {
			return pendings[i].SessionID < pendings[j].SessionID
		}
		return pendings[i].UpdatedAt.After(pendings[j].UpdatedAt)
	})
	pendingRoots := []ContentHash{}
	for _, p := range pendings {
		if !p.Dismissed && !shared[p.Target] && !inHold[p.Target] {
			out.OrphanSessions = append(out.OrphanSessions, p.SessionID)
			pendingRoots = append(pendingRoots, p.Target)
			out.HoldCounts[p.Branch]++
		}
	}
	for _, c := range hold {
		names := map[string]bool{}
		for _, u := range c.Tips {
			names[u.Branch] = true
		}
		for b := range names {
			out.HoldCounts[b] += len(c.IDs)
		}
	}
	pendingPath := x.roots(pendingRoots)
	uncommitted := graphSubset(pendingPath, func(id ContentHash) bool {
		return kept[id] && !shared[id] && !inHold[id] && strings.HasPrefix(x.byID[id].Message, "hook: ")
	})
	committed := graphSubset(kept, func(id ContentHash) bool { return !strings.HasPrefix(x.byID[id].Message, "hook: ") || shared[id] })
	visibleRoots := graphIDs(committed)
	visibleRoots = append(visibleRoots, graphIDs(shared)...)
	visibleRoots = append(visibleRoots, graphIDs(inHold)...)
	visibleRoots = append(visibleRoots, pendingRoots...)
	for _, r := range v.Refs {
		if r.Kind == RefTag {
			visibleRoots = append(visibleRoots, r.Target)
		}
	}
	for _, e := range v.History {
		visibleRoots = append(visibleRoots, e.Source, e.Target, e.SharedTarget, e.MemorySource)
	}
	visible := graphSubset(x.roots(visibleRoots), func(id ContentHash) bool { return kept[id] })
	lifecycle, err := BranchLifecycleStates(v.Refs)
	if err != nil {
		return out, err
	}
	active := x.roots(activeRoots)
	primaryReach := x.roots([]ContentHash{branches[primary].Target})
	archiveRoots := []ContentHash{}
	semantics := v.Semantics
	if semantics.Version == 0 {
		semantics = ProjectContextSemantics(v.Snapshots, v.History)
	}
	completed := map[string]bool{}
	for _, f := range semantics.Merges {
		if f.Completed {
			completed[f.EventID] = true
		}
	}
	for name, state := range lifecycle {
		if state.State != BranchArchived {
			continue
		}
		if _, ok := branches[name]; ok {
			continue
		}
		joined := false
		identities := []ContextBranch{}
		for _, b := range bindings.ByID {
			if b.Name == name {
				identities = append(identities, b)
			}
		}
		if len(identities) == 1 && identities[0].Archived {
			for _, e := range v.History {
				if completed[e.ID] && e.SourceBranchID == identities[0].ID && e.Source == state.Target {
					joined = true
				}
			}
		}
		if len(identities) == 0 {
			for _, s := range v.Snapshots {
				if !primaryReach[s.ID] || s.Branch == name || x.byID[state.Target].Branch != name {
					continue
				}
				for _, p := range s.GraftParents {
					if p == state.Target {
						joined = true
					}
				}
			}
		}
		kind := "archived"
		if joined && primaryReach[state.Target] {
			kind = "joined"
		} else {
			archiveRoots = append(archiveRoots, state.Target)
		}
		unique := 0
		for id := range x.from(state.Target).ids.each {
			if !active[id] && visible[id] {
				unique++
			}
		}
		out.Markers = append(out.Markers, GraphBranchMarker{Branch: name, Target: state.Target, Kind: kind, UniqueCount: unique, TargetAvailable: visible[state.Target]})
	}
	sort.Slice(out.Markers, func(i, j int) bool { return out.Markers[i].Branch < out.Markers[j].Branch })
	archived := graphSubset(x.roots(archiveRoots), func(id ContentHash) bool { return visible[id] && !active[id] })
	tagged := graphSubset(x.roots(tagRoots), func(id ContentHash) bool { return visible[id] && !shared[id] && !uncommitted[id] })
	pushed := graphSubset(visible, func(id ContentHash) bool { return shared[id] && !archived[id] && !uncommitted[id] })
	unpushed := graphSubset(visible, func(id ContentHash) bool { return !shared[id] && !uncommitted[id] && !tagged[id] })
	ahead := graphSubset(visible, func(id ContentHash) bool { return !shared[id] })
	tips := graphSubset(ahead, func(ContentHash) bool { return true })
	for id := range ahead {
		for _, p := range x.byID[id].ReachabilityParents() {
			delete(tips, p)
		}
	}
	for _, e := range v.History {
		if e.Kind != "position" || !visible[e.Target] {
			continue
		}
		b := bindings.ByID[e.BranchID]
		name := b.Name
		if name == "" {
			name = e.Branch
		}
		out.Positions = append(out.Positions, GraphPosition{EventID: e.ID, Branch: name, BranchID: e.BranchID, Snapshot: e.Target, Archived: b.Archived, CreatedAt: e.CreatedAt})
	}
	sort.Slice(out.Positions, func(i, j int) bool {
		if out.Positions[i].CreatedAt.Equal(out.Positions[j].CreatedAt) {
			return out.Positions[i].EventID < out.Positions[j].EventID
		}
		return out.Positions[i].CreatedAt.After(out.Positions[j].CreatedAt)
	})
	var position *GraphPosition
	for i := range out.Positions {
		if out.Positions[i].EventID == positionID {
			position = &out.Positions[i]
		}
	}
	if positionID != "" && position == nil {
		return out, fmt.Errorf("%w: selected graph position is unavailable", ErrNotFound)
	}
	out.BranchSnapshots = map[string][]ContentHash{}
	for name, ref := range branches {
		out.BranchSnapshots[name] = x.from(ref.Target).ids.list()
	}
	out.Continuations = map[ContentHash]string{}
	for _, p := range pendings {
		if p.Dismissed || shared[p.Target] {
			continue
		}
		head, ok := branches[p.Branch]
		if !ok {
			continue
		}
		ahead := false
		for _, u := range v.Unsync {
			if u.Branch == p.Branch && u.Target != head.Target {
				ahead = true
			}
		}
		if !ahead && x.byID[head.Target].SessionID != "" && x.byID[head.Target].SessionID == p.SessionID {
			out.Continuations[head.Target] = p.SessionID
		}
	}
	out.Operations = graphOperations(v, x, out, semantics)
	out.Previous = graphProgress(v, x, bindings, visible, position)
	out.SnapshotIDs = graphIDs(kept)
	out.GraphIDs = graphIDs(visible)
	out.CommittedIDs = graphIDs(committed)
	out.HistoricalIDs = graphIDs(historical)
	out.SharedIDs = graphIDs(shared)
	out.PushedIDs = graphIDs(pushed)
	out.UnpushedIDs = graphIDs(unpushed)
	out.UncommittedIDs = graphIDs(uncommitted)
	out.TaggedIDs = graphIDs(tagged)
	out.ArchivedOnlyIDs = graphIDs(archived)
	out.AheadIDs = graphIDs(ahead)
	out.AheadTips = graphIDs(tips)
	return out, nil
}

func graphHold(unsync []Unsync, x *graphStateIndex, shared, kept graphSet) ([]GraphHoldCluster, graphSet) {
	hold := graphSet{}
	stack := []ContentHash{}
	for _, u := range unsync {
		stack = append(stack, u.Target)
	}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if hold[id] || shared[id] || !kept[id] {
			continue
		}
		hold[id] = true
		stack = append(stack, x.byID[id].ReachabilityParents()...)
	}
	roots := map[ContentHash]ContentHash{}
	for id := range hold {
		roots[id] = id
	}
	find := func(id ContentHash) ContentHash {
		r := id
		for roots[r] != r {
			r = roots[r]
		}
		for roots[id] != r {
			next := roots[id]
			roots[id] = r
			id = next
		}
		return r
	}
	for id := range hold {
		for _, p := range x.byID[id].ReachabilityParents() {
			if hold[p] {
				roots[find(id)] = find(p)
			}
		}
	}
	groups := map[ContentHash][]ContentHash{}
	for id := range hold {
		root := find(id)
		groups[root] = append(groups[root], id)
	}
	out := []GraphHoldCluster{}
	for root, ids := range groups {
		sort.Slice(ids, func(i, j int) bool {
			a, b := x.byID[ids[i]], x.byID[ids[j]]
			if a.CreatedAt.Equal(b.CreatedAt) {
				return ids[i] < ids[j]
			}
			return a.CreatedAt.After(b.CreatedAt)
		})
		tips := []Unsync{}
		for _, u := range unsync {
			if hold[u.Target] && find(u.Target) == root {
				tips = append(tips, u)
			}
		}
		sort.Slice(tips, func(i, j int) bool {
			if tips[i].UpdatedAt.Equal(tips[j].UpdatedAt) {
				return tips[i].User+"/"+tips[i].Branch < tips[j].User+"/"+tips[j].Branch
			}
			return tips[i].UpdatedAt.After(tips[j].UpdatedAt)
		})
		out = append(out, GraphHoldCluster{Tips: tips, IDs: ids})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		at, bt := x.byID[a.IDs[0]].CreatedAt, x.byID[b.IDs[0]].CreatedAt
		if len(a.Tips) > 0 {
			at = a.Tips[0].UpdatedAt
		}
		if len(b.Tips) > 0 {
			bt = b.Tips[0].UpdatedAt
		}
		if at.Equal(bt) {
			return a.IDs[0] < b.IDs[0]
		}
		return at.After(bt)
	})
	return out, hold
}
