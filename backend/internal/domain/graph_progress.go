package domain

import (
	"encoding/json"
	"sort"
	"time"
)

// Retained progress is a fact of recorded movement and complete ancestry. The
// browser may expand a group, but cannot invent a reset from a timestamp.
func graphProgress(v RepositoryView, x *graphStateIndex, bindings ContextBranchProjection, visible graphSet, position *GraphPosition) []GraphProgressGroup {
	keyOf := func(name, identity string) string {
		if _, ok := bindings.ByID[identity]; ok {
			return identity
		}
		if b, ok := bindings.Active[name]; ok {
			return b.ID
		}
		return name
	}
	type movement struct {
		branch    string
		old, next ContentHash
		at        time.Time
	}
	movements := []movement{}
	for _, r := range v.Reflog {
		if r.Kind != RefBranch {
			continue
		}
		if _, ok := bindings.Active[r.Name]; !ok {
			movements = append(movements, movement{r.Name, r.Old, r.New, r.CreatedAt})
		}
	}
	for _, e := range v.History {
		if e.Kind == "advance" && e.Source != "" && e.Target != "" {
			movements = append(movements, movement{keyOf(e.Branch, e.BranchID), e.Source, e.Target, e.CreatedAt})
		}
	}
	heads := map[string]ContentHash{}
	protectedRoots := []ContentHash{}
	for _, r := range v.Refs {
		if r.Kind == RefBranch {
			heads[keyOf(r.Name, r.BranchID)] = r.Target
		}
		if r.Kind == RefSession {
			protectedRoots = append(protectedRoots, r.Target)
		}
	}
	if position != nil {
		branch := keyOf(position.Branch, position.BranchID)
		roots := map[ContentHash]time.Time{}
		if id := heads[branch]; id != "" {
			roots[id] = time.Time{}
		}
		for _, e := range v.History {
			if (position.BranchID != "" && e.BranchID != position.BranchID) || (position.BranchID == "" && keyOf(e.Branch, e.BranchID) != branch) {
				continue
			}
			for _, id := range []ContentHash{e.Source, e.Target, e.SharedTarget} {
				if id != "" {
					roots[id] = e.CreatedAt
				}
			}
		}
		ids := make([]ContentHash, 0, len(roots))
		for id := range roots {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		for _, id := range ids {
			movements = append(movements, movement{branch, id, position.Snapshot, roots[id]})
		}
		heads[branch] = position.Snapshot
	}
	out := []GraphProgressGroup{}
	seen := map[string]bool{}
	candidates := graphSet{}
	for _, m := range movements {
		if m.old == "" || m.next == "" || m.old == m.next || !visible[m.old] || !visible[m.next] {
			continue
		}
		head := heads[m.branch]
		if head == "" {
			continue
		}
		current := x.from(head)
		if !current.complete || current.ids.has(m.old) || !current.ids.has(m.next) {
			continue
		}
		before, after := x.from(m.old), x.from(m.next)
		if !before.complete || !after.complete || after.ids.has(m.old) {
			continue
		}
		raw, _ := json.Marshal([]string{m.branch, string(m.old), string(m.next)})
		key := string(raw)
		if seen[key] {
			continue
		}
		seen[key] = true
		ids := before.ids.subset(func(id ContentHash) bool { return visible[id] && !current.ids.has(id) })
		graphUnion(candidates, ids)
		branch := m.branch
		if b, ok := bindings.ByID[branch]; ok {
			branch = b.Name
		}
		out = append(out, GraphProgressGroup{Key: key, Branch: branch, Before: m.old, After: m.next, CreatedAt: m.at, SnapshotIDs: graphIDs(ids), CollapsibleIDs: []ContentHash{}})
	}
	for id := range visible {
		if !candidates[id] {
			protectedRoots = append(protectedRoots, id)
		}
	}
	for _, id := range heads {
		protectedRoots = append(protectedRoots, id)
	}
	protected := x.roots(protectedRoots)
	for i := range out {
		for _, id := range out[i].SnapshotIDs {
			if !protected[id] {
				out[i].CollapsibleIDs = append(out[i].CollapsibleIDs, id)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].Key < out[j].Key
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}
