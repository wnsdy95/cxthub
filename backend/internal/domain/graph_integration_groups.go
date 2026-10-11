package domain

import "sort"

func groupIntegrationOperations(graph *GraphState) map[string]GraphMerge {
	operations := map[string]GraphMerge{}
	for _, merge := range graph.Operations.Merges {
		operations[merge.EventID] = merge
	}
	groups := map[string]map[string]GraphMerge{}
	included := map[string]bool{}
	for _, inclusion := range graph.BranchContexts {
		for _, merge := range inclusion.Merges {
			operation, exists := operations[merge.EventID]
			if !exists || merge.MergeSHA == "" {
				continue
			}
			if operation.Scope == "" || (merge.DestinationBranchID != "" && merge.DestinationBranchID != operation.Scope) {
				continue
			}
			key := operation.Scope + "\x00" + merge.MergeSHA
			if groups[key] == nil {
				groups[key] = map[string]GraphMerge{}
			}
			groups[key][merge.EventID] = operation
			included[merge.EventID] = included[merge.EventID] || merge.State == "included"
		}
	}
	aliases := map[string]GraphMerge{}
	for key, group := range groups {
		if len(group) < 2 {
			continue
		}
		names := make([]string, 0, len(group))
		for event := range group {
			names = append(names, event)
		}
		sort.Slice(names, func(left, right int) bool {
			if included[names[left]] != included[names[right]] {
				return included[names[left]]
			}
			if (group[names[left]].PRNumber > 0) != (group[names[right]].PRNumber > 0) {
				return group[names[left]].PRNumber > 0
			}
			if len(names[left]) != len(names[right]) {
				return len(names[left]) < len(names[right])
			}
			return names[left] < names[right]
		})
		canonical := group[names[0]]
		canonical.ID = "graph:merge:contributions:" + string(HashContent([]byte(key)))
		sources := graphSet{}
		for _, name := range names {
			sources[group[name].Source] = true
		}
		canonical.SourceArms = graphIDs(sources)
		for _, name := range names {
			operation := operations[name]
			operation.ID = canonical.ID
			operations[name] = operation
			aliases[name] = canonical
		}
	}
	kept := make([]GraphMerge, 0, len(graph.Operations.Merges))
	seen := map[string]bool{}
	for _, operation := range graph.Operations.Merges {
		if alias, exists := aliases[operation.EventID]; exists {
			operation = alias
		}
		if !seen[operation.ID] {
			kept = append(kept, operation)
			seen[operation.ID] = true
		}
	}
	graph.Operations.Merges = kept
	return operations
}
