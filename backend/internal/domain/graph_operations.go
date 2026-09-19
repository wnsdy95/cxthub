package domain

import (
	"encoding/json"
	"sort"
	"time"
)

// GraphOperations records which operations and edge substitutions are supported
// by repository evidence. The browser only places these nodes and routes lines;
// none of these display substitutions modifies stored conversation ancestry.
type GraphOperations struct {
	Births []GraphBirth `json:"births"`
	Merges []GraphMerge `json:"merges"`
}
type GraphBirth struct {
	ID        string        `json:"id"`
	EventID   string        `json:"event_id"`
	Branch    string        `json:"branch"`
	Source    ContentHash   `json:"source"`
	Orphan    bool          `json:"orphan"`
	CreatedAt time.Time     `json:"created_at"`
	Children  []ContentHash `json:"children"`
}
type GraphMerge struct {
	ID                string        `json:"id"`
	EventID           string        `json:"event_id"`
	Before            ContentHash   `json:"before"`
	After             ContentHash   `json:"after"`
	Source            ContentHash   `json:"source"`
	Branch            string        `json:"branch"`
	Scope             string        `json:"scope"`
	From              string        `json:"from"`
	CreatedAt         time.Time     `json:"created_at"`
	PRNumber          int           `json:"pr_number,omitempty"`
	HistoricalOnly    bool          `json:"historical_only"`
	Withdrawn         bool          `json:"withdrawn"`
	RepresentedGrafts []ContentHash `json:"represented_grafts"`
	RedirectChildren  []ContentHash `json:"redirect_children"`
	SourceBirth       string        `json:"source_birth,omitempty"`
	LifecycleBirth    string        `json:"lifecycle_birth,omitempty"`
	identity          string
}

func graphOperationKey(parts ...string) string { b, _ := json.Marshal(parts); return string(b) }
func graphConversationParents(s Snapshot) []ContentHash {
	p := s.Parents
	if s.Grafted && len(s.GraftParents) == 0 && len(p) > 0 {
		return p[1:]
	}
	return p
}

func graphOperations(v RepositoryView, x *graphStateIndex, state GraphState, semantics ContextSemantics) GraphOperations {
	out := GraphOperations{Births: []GraphBirth{}, Merges: []GraphMerge{}}
	scopeOf := func(name string) string {
		if scope := state.RefScopes[name]; scope != "" {
			return scope
		}
		if scope := state.SnapshotScopes[name]; scope != "" {
			return scope
		}
		return "legacy:" + name
	}
	reaches := func(root, target ContentHash) bool { return target != "" && x.from(root).ids.has(target) }
	events := map[string]HistoryEvent{}
	birthCounts := map[string]int{}
	births := map[string]HistoryEvent{}
	branchTips := map[ContentHash]map[string]bool{}
	addTip := func(id ContentHash, name string) {
		if branchTips[id] == nil {
			branchTips[id] = map[string]bool{}
		}
		branchTips[id][name] = true
	}
	branchTargets := map[string]ContentHash{}
	for _, r := range v.Refs {
		if lifecycle, ok, _ := ParseBranchLifecycleRef(r); ok {
			addTip(r.Target, lifecycle.Branch)
		} else if r.Kind == RefBranch {
			addTip(r.Target, r.Name)
		}
		if r.Kind == RefBranch {
			branchTargets[scopeOf(r.Name)] = r.Target
		}
	}
	for _, h := range v.History {
		events[h.ID] = h
		if h.Kind == "birth" || h.Kind == "orphan" {
			birthCounts[h.BranchID]++
		}
		if h.Kind != "pr-merge" && h.Target != "" {
			addTip(h.Target, h.Branch)
		}
	}
	for _, h := range v.History {
		if h.Kind == "birth" && birthCounts[h.BranchID] == 1 && h.Source != "" && h.Source == h.Target {
			if _, ok := x.byID[h.Source]; ok {
				births[h.BranchID] = h
			}
		}
	}
	completed := []HistoryEvent{}
	facts := map[string]ContextMergeEvidence{}
	completedMoves := map[string]bool{}
	for _, f := range semantics.Merges {
		h := events[f.EventID]
		_, before := x.byID[h.SharedTarget]
		_, after := x.byID[h.Target]
		if !f.Completed || !f.SourceAvailable || !before || !after || h.PR == nil {
			continue
		}
		completed = append(completed, h)
		facts[h.ID] = f
		completedMoves[graphOperationKey(h.BranchID, string(h.SharedTarget), string(h.Target))] = true
	}
	publications := map[string]graphSet{}
	publicationTimes := map[string]time.Time{}
	receipts := map[string][]HistoryEvent{}
	type move struct {
		old, next ContentHash
		at        time.Time
	}
	movements := map[string][]move{}
	roots := map[string]graphSet{}
	addRoot := func(identity string, target ContentHash) {
		if identity == "" {
			return
		}
		if _, ok := x.byID[target]; !ok {
			return
		}
		if roots[identity] == nil {
			roots[identity] = graphSet{}
		}
		roots[identity][target] = true
	}
	for _, h := range v.History {
		if h.Kind == "advance" || h.Kind == "publish" {
			if publications[h.BranchID] == nil {
				publications[h.BranchID] = graphSet{}
			}
			publications[h.BranchID][h.Target] = true
			key := graphOperationKey(h.Branch, string(h.Target))
			prior, ok := publicationTimes[key]
			if !ok || h.CreatedAt.Before(prior) {
				publicationTimes[key] = h.CreatedAt
			}
			addRoot(h.BranchID, h.Target)
		}
		if h.Kind == "advance" {
			movements[h.BranchID] = append(movements[h.BranchID], move{h.Source, h.Target, h.CreatedAt})
		}
		if h.Kind == "pr-merge" && !h.PRCompleted && h.PR != nil {
			key := graphOperationKey(h.Branch, string(h.Source), string(h.SharedTarget))
			receipts[key] = append(receipts[key], h)
		}
	}
	for _, r := range v.Reflog {
		if r.Kind == RefBranch {
			key := scopeOf(r.Name)
			movements[key] = append(movements[key], move{r.Old, r.New, r.CreatedAt})
		}
	}
	for _, h := range completed {
		addRoot(h.SourceBranchID, h.Source)
		name := h.Branch
		if b, ok := state.BranchHeads[h.BranchID]; ok {
			name = b.Branch
		}
		out.Merges = append(out.Merges, GraphMerge{ID: "graph:merge:" + h.ID, EventID: h.ID, Before: h.SharedTarget, After: h.Target, Source: h.Source,
			Branch: name, Scope: h.BranchID, From: h.PR.HeadBranch, CreatedAt: h.CreatedAt, PRNumber: h.PR.Number,
			HistoricalOnly: !facts[h.ID].PlacementIntact, identity: h.SourceBranchID})
	}
	// Legacy evidence requires a recorded forward movement plus an unambiguous
	// receipt or explicit append edge. Names reused by identities are insufficient.
	claims := map[string]map[string]bool{}
	for _, h := range v.History {
		if claims[h.Branch] == nil {
			claims[h.Branch] = map[string]bool{}
		}
		claims[h.Branch][h.BranchID] = true
	}
	for _, r := range v.Reflog {
		if r.Kind != RefBranch || len(claims[r.Name]) > 1 || r.Old == "" || r.New == "" || r.Old == r.New || !reaches(r.New, r.Old) {
			continue
		}
		if _, ok := x.byID[r.Old]; !ok {
			continue
		}
		if _, ok := x.byID[r.New]; !ok {
			continue
		}
		rs := []HistoryEvent{}
		for _, h := range receipts[graphOperationKey(r.Name, string(r.New), string(r.Old))] {
			if !h.CreatedAt.After(r.CreatedAt) {
				rs = append(rs, h)
			}
		}
		if len(rs) > 1 || completedMoves[graphOperationKey(scopeOf(r.Name), string(r.Old), string(r.New))] {
			continue
		}
		appendEdge := false
		incoming, previous := x.from(r.New).ids, x.from(r.Old).ids
		for id := range incoming.each {
			if previous.has(id) {
				continue
			}
			for _, p := range x.byID[id].GraftParents {
				if p == r.Old {
					appendEdge = true
				}
			}
		}
		publication, published := publicationTimes[graphOperationKey(r.Name, string(r.New))]
		if len(rs) != 1 && (!appendEdge || x.byID[r.New].Branch == r.Name || (published && !publication.After(r.CreatedAt))) {
			continue
		}
		from, identity := "", ""
		if len(rs) == 1 {
			from, identity = rs[0].PR.HeadBranch, rs[0].SourceBranchID
		} else {
			candidates := []string{}
			for name := range branchTips[r.New] {
				if name != r.Name {
					candidates = append(candidates, name)
				}
			}
			if len(candidates) == 1 {
				from = candidates[0]
			}
		}
		if from == "" {
			continue
		}
		out.Merges = append(out.Merges, GraphMerge{ID: "graph:merge:" + r.CreatedAt.Format(time.RFC3339Nano) + ":" + r.Name + ":" + string(r.Old) + ":" + string(r.New), EventID: "ref-move",
			Before: r.Old, After: r.New, Source: r.New, Branch: r.Name, Scope: scopeOf(r.Name), From: from, CreatedAt: r.CreatedAt, identity: identity})
	}
	sort.Slice(out.Merges, func(i, j int) bool {
		a, b := out.Merges[i], out.Merges[j]
		if a.CreatedAt.Equal(b.CreatedAt) {
			return a.ID < b.ID
		}
		return a.CreatedAt.Before(b.CreatedAt)
	})
	for i := range out.Merges {
		m := &out.Merges[i]
		m.RepresentedGrafts = []ContentHash{}
		m.RedirectChildren = []ContentHash{}
		for _, r := range movements[m.Scope] {
			if r.at.After(m.CreatedAt) && r.next != "" && reaches(r.old, m.After) && !reaches(r.next, r.old) {
				m.Withdrawn = true
			}
		}
		segment, previous := x.from(m.Source).ids, x.from(m.Before).ids
		for id := range segment.each {
			if !previous.has(id) {
				for _, p := range x.byID[id].GraftParents {
					if p == m.Before {
						m.RepresentedGrafts = append(m.RepresentedGrafts, id)
					}
				}
			}
		}
		activeIDs := x.from(branchTargets[m.Scope]).ids
		for _, s := range v.Snapshots {
			if m.HistoricalOnly || m.Withdrawn || segment.has(s.ID) || (!publications[m.Scope][s.ID] && state.SnapshotScopes[s.Branch] != m.Scope) || !activeIDs.has(s.ID) {
				continue
			}
			for _, p := range s.Parents {
				if p == m.After {
					m.RedirectChildren = append(m.RedirectChildren, s.ID)
					break
				}
			}
		}
		sort.Slice(m.RepresentedGrafts, func(i, j int) bool { return m.RepresentedGrafts[i] < m.RepresentedGrafts[j] })
		sort.Slice(m.RedirectChildren, func(i, j int) bool { return m.RedirectChildren[i] < m.RedirectChildren[j] })
	}
	// Resolve ownership before rendering. A shared content hash claimed by two
	// births must not arbitrarily become a child of whichever event arrived first.
	childClaims := map[ContentHash]map[ContentHash]map[string]bool{}
	claim := func(child, parent ContentHash, birth string) {
		if childClaims[child] == nil {
			childClaims[child] = map[ContentHash]map[string]bool{}
		}
		if childClaims[child][parent] == nil {
			childClaims[child][parent] = map[string]bool{}
		}
		childClaims[child][parent][birth] = true
	}
	for _, h := range v.History {
		_, normal := births[h.BranchID]
		if (!normal || h.Kind != "birth") && (h.Kind != "orphan" || birthCounts[h.BranchID] != 1) {
			continue
		}
		b := GraphBirth{ID: "graph:birth:" + h.ID, EventID: h.ID, Branch: h.Branch, Source: h.Source, CreatedAt: h.CreatedAt, Children: []ContentHash{}, Orphan: h.Kind == "orphan"}
		if b.Orphan {
			for _, id := range graphIDs(roots[h.BranchID]) {
				if len(x.byID[id].Parents) == 0 {
					if b.Source == "" {
						b.Source = id
					}
					claim(id, "", b.ID)
				}
			}
			if b.Source == "" {
				continue
			}
		} else {
			seen := graphSet{}
			stack := graphIDs(roots[h.BranchID])
			for len(stack) > 0 {
				id := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				if id == h.Source || seen[id] {
					continue
				}
				seen[id] = true
				s, ok := x.byID[id]
				if !ok {
					continue
				}
				for _, p := range graphConversationParents(s) {
					if p == h.Source {
						claim(id, h.Source, b.ID)
					}
					stack = append(stack, p)
				}
			}
		}
		out.Births = append(out.Births, b)
	}
	for i := range out.Births {
		b := &out.Births[i]
		parent := b.Source
		if b.Orphan {
			parent = ""
		}
		for child, parents := range childClaims {
			if cs := parents[parent]; len(cs) == 1 && cs[b.ID] {
				b.Children = append(b.Children, child)
			}
		}
		sort.Slice(b.Children, func(i, j int) bool { return b.Children[i] < b.Children[j] })
		if b.Orphan {
			continue
		}
		birth := events[b.EventID]
		for j := range out.Merges {
			m := &out.Merges[j]
			if m.CreatedAt.Before(b.CreatedAt) {
				continue
			}
			if m.Source == b.Source {
				matches := 0
				for _, other := range births {
					if other.Branch == m.From && !other.CreatedAt.After(m.CreatedAt) {
						matches++
					}
				}
				if m.identity == birth.BranchID || (m.identity == "" && m.From == b.Branch && matches == 1) {
					m.SourceBirth = b.ID
				}
			}
			if f, ok := facts[m.EventID]; ok && f.BirthID == b.EventID {
				m.LifecycleBirth = b.ID
			}
		}
	}
	sort.Slice(out.Births, func(i, j int) bool { return out.Births[i].ID < out.Births[j].ID })
	return out
}
