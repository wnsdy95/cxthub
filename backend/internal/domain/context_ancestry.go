package domain

// A DFS subtree is a sufficient (never necessary) reachability proof. Building
// it from tips makes ordinary session chains O(V+E), instead of walking the
// whole chain again for every historical PR. Cross edges use exact traversal.
type contextInterval struct{ start, end int }
type contextAncestry struct {
	nodes map[ContentHash]Snapshot
	edges map[ContentHash][]ContentHash
	tree  map[ContentHash]contextInterval
}

func newContextAncestry(nodes map[ContentHash]Snapshot, conversation bool) contextAncestry {
	a := contextAncestry{nodes: nodes, edges: make(map[ContentHash][]ContentHash, len(nodes)), tree: make(map[ContentHash]contextInterval, len(nodes))}
	nonTips := make(map[ContentHash]bool)
	for id, s := range nodes {
		parents := s.ReachabilityParents()
		if conversation {
			parents = s.Parents
			if s.Grafted && len(s.GraftParents) == 0 && len(parents) > 0 {
				parents = parents[1:]
			}
		}
		a.edges[id] = parents
		for _, p := range parents {
			nonTips[p] = true
		}
	}
	type frame struct {
		id   ContentHash
		next int
	}
	clock := 0
	visit := func(root ContentHash) {
		if _, ok := a.tree[root]; ok {
			return
		}
		a.tree[root] = contextInterval{start: clock}
		clock++
		stack := []frame{{id: root}}
		for len(stack) > 0 {
			f := &stack[len(stack)-1]
			if f.next == len(a.edges[f.id]) {
				v := a.tree[f.id]
				v.end = clock
				a.tree[f.id] = v
				stack = stack[:len(stack)-1]
				continue
			}
			p := a.edges[f.id][f.next]
			f.next++
			if _, ok := nodes[p]; !ok {
				continue
			}
			if _, ok := a.tree[p]; ok {
				continue
			}
			a.tree[p] = contextInterval{start: clock}
			clock++
			stack = append(stack, frame{id: p})
		}
	}
	for id := range nodes {
		if !nonTips[id] {
			visit(id)
		}
	}
	for id := range nodes {
		visit(id)
	}
	return a
}

func (a contextAncestry) reaches(root, target ContentHash) bool {
	r, rok := a.tree[root]
	t, tok := a.tree[target]
	if !rok || !tok {
		return false
	}
	if r.start <= t.start && t.start < r.end {
		return true
	}
	seen := map[ContentHash]bool{}
	stack := []ContentHash{root}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if id == target {
			return true
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		stack = append(stack, a.edges[id]...)
	}
	return false
}
