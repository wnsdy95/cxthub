package app

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// Persist the identity transition before moving any mutable name. Existing
// lifecycle retry machinery can then finish projection without inventing a
// second identity or losing the original operation after a crash.
func recordBranchTransition(ctx context.Context, store outbound.SessionStore, repo, from, to string, target domain.ContentHash) error {
	history, ok := store.(outbound.HistoryStore)
	if !ok {
		return nil
	}
	events, err := history.ListHistoryEvents(ctx, repo)
	if err != nil {
		return err
	}
	state, err := domain.ProjectContextBranches(events)
	if err != nil {
		return err
	}
	if released := state.Released[from]; released != "" {
		if _, active := state.Active[from]; !active {
			for _, e := range events {
				if e.ID == released && e.Target == target && ((to == "" && e.Kind == "archive") || (e.Kind == "rename" && e.Branch == to)) {
					return nil
				}
			}
		}
	}
	identity := state.Identity(repo, from)
	parent := state.Active[from].EventID
	kind, name := "rename", to
	if to == "" {
		kind, name = "archive", from
	}
	key := sha256.Sum256([]byte(repo + "\x00" + identity + "\x00" + parent + "\x00" + kind + "\x00" + from + "\x00" + to + "\x00" + string(target)))
	e := domain.HistoryEvent{ID: fmt.Sprintf("%x", key[:16]), RepoID: repo, BranchID: identity, Branch: name, Kind: kind, Source: target, Target: target, BindingParent: parent, CreatedAt: time.Now().UTC()}
	if kind == "rename" {
		e.PreviousBranch = from
		e.NameParent = state.Released[to]
	}
	if _, err := domain.ProjectContextBranches(append(events, e)); err != nil {
		return err
	}
	return history.PutHistoryEvent(ctx, e)
}

// Explicit restoration starts a new active identity after the recorded release.
// The old identity and its conversations remain available as archived history.
func recordRestoredBranch(ctx context.Context, store outbound.SessionStore, repo, branch string, target domain.ContentHash) error {
	history, ok := store.(outbound.HistoryStore)
	if !ok {
		return nil
	}
	if repo == "" {
		snap, err := store.GetSnapshot(ctx, target)
		if err != nil {
			return err
		}
		repo = snap.RepoID
	}
	events, err := history.ListHistoryEvents(ctx, repo)
	if err != nil {
		return err
	}
	state, err := domain.ProjectContextBranches(events)
	if err != nil {
		return err
	}
	if _, active := state.Active[branch]; active || state.Released[branch] == "" {
		return nil
	}
	parent := state.Released[branch]
	key := sha256.Sum256([]byte(repo + "\x00restore\x00" + branch + "\x00" + parent + "\x00" + string(target)))
	id := fmt.Sprintf("%x", key[:16])
	e := domain.HistoryEvent{ID: id, RepoID: repo, BranchID: id, Branch: branch, Kind: "birth", Source: target, Target: target, BindingParent: parent, CreatedAt: time.Now().UTC()}
	e, err = NewContextHistoryService(store, history).ValidateHistorySource(ctx, e)
	if err != nil {
		return err
	}
	return history.PutHistoryEvent(ctx, e)
}
