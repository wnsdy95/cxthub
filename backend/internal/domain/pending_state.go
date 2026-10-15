package domain

import (
	"context"
	"fmt"
	"sort"
)

type PendingStateView struct {
	Projection       string               `json:"projection"`
	Revision         RepositoryRevision   `json:"revision"`
	DefaultBranch    string               `json:"default_branch"`
	Refs             []Ref                `json:"refs"`
	Pending          []Pending            `json:"pending"`
	Snapshots        []Snapshot           `json:"snapshots"`
	ArchivedSessions []SessionArchiveView `json:"archived_sessions"`
	Hold             []GraphHoldCluster   `json:"hold"`
	OrphanSessions   []string             `json:"orphan_sessions"`
	HoldCounts       map[string]int       `json:"hold_counts"`
}

type pendingClassification struct {
	historical, shared, kept, inHold graphSet
	hold                             []GraphHoldCluster
	pendings                         []Pending
	roots                            []ContentHash
	orphans                          []string
	counts                           map[string]int
}

func classifyPending(view RepositoryView, index *graphStateIndex, sharedRoots []ContentHash) pendingClassification {
	historicalRoots := []ContentHash{}
	for _, event := range view.Reflog {
		if event.Kind == RefBranch {
			historicalRoots = append(historicalRoots, event.Old, event.New)
		}
	}
	for _, event := range view.History {
		switch event.Kind {
		case "advance":
			historicalRoots = append(historicalRoots, event.Source, event.Target)
		case "publish":
			historicalRoots = append(historicalRoots, event.Target)
		case "pr-merge":
			if event.PRCompleted {
				historicalRoots = append(historicalRoots, event.Source, event.Target, event.SharedTarget)
			}
		}
	}
	out := pendingClassification{historical: index.roots(historicalRoots), shared: index.roots(sharedRoots), kept: graphSet{}, roots: []ContentHash{}, orphans: []string{}, counts: map[string]int{}}
	graphUnion(out.shared, out.historical)
	for _, snapshot := range view.Snapshots {
		if snapshot.Branch != "(stash)" || out.shared[snapshot.ID] {
			out.kept[snapshot.ID] = true
		}
	}
	out.hold, out.inHold = graphHold(view.Unsync, index, out.shared, out.kept)
	out.pendings = append([]Pending{}, view.Pending...)
	sort.Slice(out.pendings, func(left, right int) bool {
		if out.pendings[left].UpdatedAt.Equal(out.pendings[right].UpdatedAt) {
			return out.pendings[left].SessionID < out.pendings[right].SessionID
		}
		return out.pendings[left].UpdatedAt.After(out.pendings[right].UpdatedAt)
	})
	for _, pending := range out.pendings {
		if !pending.Dismissed && !out.shared[pending.Target] && !out.inHold[pending.Target] {
			out.orphans = append(out.orphans, pending.SessionID)
			out.roots = append(out.roots, pending.Target)
			out.counts[pending.Branch]++
		}
	}
	for _, cluster := range out.hold {
		branches := map[string]bool{}
		for _, tip := range cluster.Tips {
			branches[tip.Branch] = true
		}
		for branch := range branches {
			out.counts[branch] += len(cluster.IDs)
		}
	}
	return out
}

func ProjectPendingState(ctx context.Context, view RepositoryView) (PendingStateView, error) {
	if err := ctx.Err(); err != nil {
		return PendingStateView{}, err
	}
	index, err := newGraphStateIndexContext(ctx, view.Snapshots)
	if err != nil {
		return PendingStateView{}, err
	}
	if _, err := ProjectContextBranches(view.History); err != nil {
		return PendingStateView{}, fmt.Errorf("%w: %v", ErrIntegrity, err)
	}
	sharedRoots := []ContentHash{}
	for _, ref := range view.Refs {
		_, lifecycle, err := ParseBranchLifecycleRef(ref)
		if err != nil {
			return PendingStateView{}, err
		}
		if ref.Kind == RefBranch || ref.Kind == RefSession || lifecycle {
			sharedRoots = append(sharedRoots, ref.Target)
		}
	}
	classification := classifyPending(view, index, sharedRoots)
	out := PendingStateView{Projection: "on-hold-v1", Revision: view.Revision, DefaultBranch: view.DefaultBranch,
		Refs: append([]Ref{}, view.Refs...), Pending: classification.pendings, Snapshots: []Snapshot{},
		ArchivedSessions: append([]SessionArchiveView{}, view.ArchivedSessions...), Hold: classification.hold,
		OrphanSessions: classification.orphans, HoldCounts: classification.counts}
	selected := graphSet{}
	for _, pending := range out.Pending {
		if _, exists := index.byID[pending.Target]; !exists {
			return PendingStateView{}, ErrNotFound
		}
		selected[pending.Target] = true
	}
	graphUnion(selected, classification.inHold)
	for _, archive := range out.ArchivedSessions {
		for _, id := range archive.SnapshotIDs {
			selected[id] = true
		}
		for _, id := range []ContentHash{archive.SnapshotID, archive.LatestSnapshotID, archive.Origin.ParentSnapshotID, archive.Origin.MainSnapshotID} {
			selected[id] = true
		}
	}
	for _, snapshot := range view.Snapshots {
		if selected[snapshot.ID] {
			snapshot.Branches = nil
			out.Snapshots = append(out.Snapshots, snapshot)
		}
	}
	if err := ctx.Err(); err != nil {
		return PendingStateView{}, err
	}
	return out, nil
}
