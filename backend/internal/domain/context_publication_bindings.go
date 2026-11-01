package domain

import "sort"

func ContextPublicationBindings(repo ContentHash, history []HistoryEvent) map[ContentHash][]CommitContextBinding {
	type proofKey struct {
		repo, branchID, branch, local, worktree, git string
		target                                       ContentHash
	}
	key := func(event HistoryEvent) proofKey {
		return proofKey{event.RepoID, event.BranchID, event.Branch, event.LocalBranch, event.WorktreeID, event.GitAfter, event.Target}
	}
	observed := map[proofKey]HistoryEvent{}
	for _, event := range history {
		if event.RepoID == string(repo) && event.Kind != "publish" && event.Kind != "pr-merge" && ValidateHistoryEvent(event) == nil {
			observed[key(event)] = event
		}
	}
	out := map[ContentHash][]CommitContextBinding{}
	for _, event := range history {
		if event.RepoID != string(repo) || event.Kind != "publish" || ValidateHistoryEvent(event) != nil || !IsPublicationProof(event, observed[key(event)]) {
			continue
		}
		out[event.Target] = append(out[event.Target], CommitContextBinding{EventID: event.ID, GitCommit: event.GitAfter, BranchID: event.BranchID, WorktreeID: event.WorktreeID, SegmentIDs: []ContentHash{}})
	}
	for _, entries := range out {
		sort.Slice(entries, func(first, second int) bool { return entries[first].EventID < entries[second].EventID })
	}
	return out
}
