package domain

import "slices"

// Preserve the destination's observed first-parent continuation between Git-
// ordered PRs. Branch births still point at their recorded Source: neither a
// lane label, capture timestamp nor the destination name can move that fork.
func routeIntegrationContinuations(graph *GraphState, snapshots map[ContentHash]Snapshot, merges map[string]GraphMerge) {
	type route struct {
		plan     int
		merge    string // empty for a current continuation after the final PR
		previous string
		root     string
		tail     string
		parents  []string
	}
	var routes []route
	sources := map[ContentHash]bool{}
	projected := map[string][]string{}
	for _, m := range merges {
		sources[m.Source] = true
	}
	for _, plan := range graph.Integrations {
		for id, parents := range plan.Parents {
			projected[id] = parents
		}
	}
	// A real capture can be shared by several branch identities. Conflicting
	// display substitutions have no unique owner and must not be chosen by the
	// order of branch names or map iteration.
	proposals := map[string][]string{}
	conflicts := map[string]bool{}
	add := func(r route) {
		if r.tail != "" {
			if prior, ok := proposals[r.tail]; ok && !slices.Equal(prior, r.parents) {
				conflicts[r.tail] = true
			}
			proposals[r.tail] = r.parents
		}
		routes = append(routes, r)
	}
	for i, plan := range graph.Integrations {
		var previous GraphMerge
		for _, included := range graph.BranchContexts[plan.Branch].Merges {
			m, ok := merges[included.EventID]
			if !ok || included.State != "included" || m.Scope != plan.Scope || m.Source != included.Source {
				continue
			}
			if previous.ID != "" && included.Before != "" && included.Before != m.Source {
				if root, tail, parents, ok := integrationContinuation(included.Before, previous, snapshots, sources); ok {
					add(route{i, m.ID, previous.ID, root, tail, parents})
				}
			}
			previous = m
		}
		if previous.ID != "" && len(plan.ExtraParents) > 0 {
			if root, tail, parents, ok := integrationContinuation(plan.Target, previous, snapshots, sources); ok && tail != "" {
				add(route{i, "", previous.ID, root, tail, parents})
			}
		}
	}
	// Include mutable grafts in the cycle fence. They cannot prove the forward
	// continuation, but can make a proposed display substitution unsafe.
	reaches := func(from, target string) bool {
		seen := map[string]bool{}
		queue := []string{from}
		for len(queue) > 0 {
			id := queue[len(queue)-1]
			queue = queue[:len(queue)-1]
			if id == target {
				return true
			}
			if seen[id] {
				continue
			}
			seen[id] = true
			s := snapshots[ContentHash(id)]
			if p, ok := projected[id]; ok {
				queue = append(queue, p...)
			} else {
				for _, p := range s.Parents {
					queue = append(queue, string(p))
				}
			}
			for _, p := range s.GraftParents {
				queue = append(queue, string(p))
			}
		}
		return false
	}
	for _, r := range routes {
		if r.tail != "" && (conflicts[r.tail] || reaches(r.previous, r.tail)) {
			continue
		}
		plan := &graph.Integrations[r.plan]
		if r.tail != "" {
			plan.Parents[r.tail] = r.parents
			projected[r.tail] = r.parents
		}
		if r.merge == "" {
			plan.ExtraParents = []string{}
		} else {
			parents := plan.Parents[r.merge]
			// Replace the previous-operation and pre-merge arms with one proven
			// destination path. The final source arm remains the PR contribution.
			plan.Parents[r.merge] = []string{r.root, parents[len(parents)-1]}
			projected[r.merge] = plan.Parents[r.merge]
		}
	}
}

// Returns a display-only substitution at the end of a natural continuation.
// Never cross another PR's source, a legacy destructive graft, or missing/
// cyclic ancestry. Unproven history keeps the conservative integration plan.
func integrationContinuation(root ContentHash, previous GraphMerge, snapshots map[ContentHash]Snapshot, sources map[ContentHash]bool) (string, string, []string, bool) {
	// A destination capture may continue its own pre-PR conversation rather
	// than the contributor's tip. Stop at the first shared natural ancestor:
	// replacing that ancestor itself would route the PR back through its own
	// source and create a cycle. Mutable grafts cannot establish this boundary.
	anchors := map[ContentHash]bool{}
	for _, start := range []ContentHash{previous.Source, previous.After} {
		for id := start; id != "" && !anchors[id]; {
			anchors[id] = true
			s, ok := snapshots[id]
			if !ok || len(s.Parents) == 0 || (s.Grafted && len(s.GraftParents) == 0) {
				break
			}
			id = s.Parents[0]
		}
	}
	isAnchor := func(id ContentHash) bool { return anchors[id] }
	if isAnchor(root) {
		return previous.ID, "", nil, true
	}
	seen := map[ContentHash]bool{}
	for id := root; id != "" && !seen[id]; {
		seen[id] = true
		s, ok := snapshots[id]
		if !ok || sources[id] || len(s.Parents) == 0 || (s.Grafted && len(s.GraftParents) == 0) {
			break
		}
		if isAnchor(s.Parents[0]) {
			parents := []string{previous.ID}
			for _, p := range s.Parents[1:] {
				parents = append(parents, string(p))
			}
			return string(root), string(id), parents, true
		}
		id = s.Parents[0]
	}
	return "", "", nil, false
}
