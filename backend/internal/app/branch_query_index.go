package app

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type branchTargetKey struct {
	branch string
	target domain.ContentHash
}

type branchHistoryIndex struct {
	repo         domain.ContentHash
	rows         []domain.HistoryEvent
	byID         map[string]int
	byBranch     map[string][]int
	byTarget     map[branchTargetKey][]int
	receipts     map[string][]int
	births       map[string][]domain.ContentHash
	publications map[domain.ContentHash][]string
}

func newBranchHistoryIndex(ctx context.Context, repo domain.ContentHash, history []domain.HistoryEvent) (*branchHistoryIndex, error) {
	index := &branchHistoryIndex{repo: repo, rows: history, byID: make(map[string]int, len(history)), byBranch: map[string][]int{}, byTarget: map[branchTargetKey][]int{}, receipts: map[string][]int{}, births: map[string][]domain.ContentHash{}, publications: map[domain.ContentHash][]string{}}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for position, event := range history {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if event.RepoID != string(repo) {
			return nil, domain.ErrIntegrity
		}
		if _, exists := index.byID[event.ID]; !exists {
			index.byID[event.ID] = position
		}
		index.byBranch[event.BranchID] = append(index.byBranch[event.BranchID], position)
		key := branchTargetKey{event.BranchID, event.Target}
		index.byTarget[key] = append(index.byTarget[key], position)
		if event.Kind == "pr-merge" && !event.PRCompleted && event.PR != nil {
			index.receipts[event.BranchID] = append(index.receipts[event.BranchID], position)
		}
		if event.Kind == "birth" && event.Source != "" {
			index.births[event.BranchID] = append(index.births[event.BranchID], event.Source)
		}
		if event.Kind == "publish" && event.Source == event.Target && event.GitAfter != "" {
			index.publications[event.Target] = append(index.publications[event.Target], event.BranchID)
		}
	}
	return index, nil
}

func (index *branchHistoryIndex) completion(receipt domain.HistoryEvent) (domain.HistoryEvent, bool, error) {
	key := sha256.Sum256([]byte(receipt.ID + ":completed"))
	position, exists := index.byID[fmt.Sprintf("%x", key[:16])]
	if !exists {
		return domain.HistoryEvent{}, false, nil
	}
	completion := index.rows[position]
	complete, err := hasPRCompletion([]domain.HistoryEvent{completion}, receipt)
	return completion, complete, err
}

func (index *branchHistoryIndex) targetEvents(branch string, target domain.ContentHash) []domain.HistoryEvent {
	positions := index.byTarget[branchTargetKey{branch, target}]
	rows := make([]domain.HistoryEvent, 0, len(positions))
	for _, position := range positions {
		rows = append(rows, index.rows[position])
	}
	return rows
}

type branchQueryInputs struct {
	history   *branchHistoryIndex
	snapshots map[domain.ContentHash]domain.Snapshot
}

func newBranchQueryInputs(ctx context.Context, repo domain.ContentHash, snapshots []domain.Snapshot, history []domain.HistoryEvent) (*branchQueryInputs, error) {
	index, err := newBranchHistoryIndex(ctx, repo, history)
	if err != nil {
		return nil, err
	}
	inputs := &branchQueryInputs{history: index, snapshots: make(map[domain.ContentHash]domain.Snapshot, len(snapshots))}
	for _, snapshot := range snapshots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if snapshot.RepoID != repo {
			return nil, domain.ErrIntegrity
		}
		if _, exists := inputs.snapshots[snapshot.ID]; exists {
			return nil, domain.ErrIntegrity
		}
		inputs.snapshots[snapshot.ID] = snapshot
	}
	return inputs, nil
}
