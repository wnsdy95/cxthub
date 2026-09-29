package app

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// prepareCheckoutRestore derives the existing restore event without publishing
// it. Checkout's journal commits it with the ref and worktree cursor.
func prepareCheckoutRestore(ctx context.Context, store outbound.SessionStore, repo, branch string, target domain.ContentHash) (*domain.HistoryEvent, error) {
	history, ok := store.(outbound.HistoryStore)
	if !ok {
		return nil, nil
	}
	if repo == "" {
		snap, err := store.GetSnapshot(ctx, target)
		if err != nil {
			return nil, err
		}
		repo = snap.RepoID
	}
	events, err := history.ListHistoryEvents(ctx, repo)
	if err != nil {
		return nil, err
	}
	state, err := domain.ProjectContextBranches(events)
	if err != nil {
		return nil, err
	}
	if _, active := state.Active[branch]; active || state.Released[branch] == "" {
		return nil, nil
	}
	parent := state.Released[branch]
	key := sha256.Sum256([]byte(repo + "\x00restore\x00" + branch + "\x00" + parent + "\x00" + string(target)))
	id := fmt.Sprintf("%x", key[:16])
	event := domain.HistoryEvent{ID: id, RepoID: repo, BranchID: id, Branch: branch, Kind: "birth", Source: target, Target: target, BindingParent: parent, CreatedAt: time.Now().UTC()}
	event, err = NewContextHistoryService(store, history).ValidateHistorySource(ctx, event)
	if err != nil {
		return nil, err
	}
	return &event, nil
}
