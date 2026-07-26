package app

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// ForkSessionService implements the use-case service for the ForkSession inbound port.
//
// Dependency outbound port: SessionStore.
//
// Authoritative path comment (SYNC-PROTOCOL §2.8): Forking is sufficient for local ref duplication, but
// for immediately reflecting team shared branches on the server, backendclient's POST /repos/{repoID}/fork
// can be used. In the scaffolding phase, only SessionStore.PutRef is used locally.
//
// Fork sequence (local, SYNC-PROTOCOL §5.3 fork mental model):
//  1. SessionStore.GetSnapshot(FromSnapshot) → existence verification
//  2. SessionStore.PutRef(Ref{Kind:branch, Name:NewBranch, Target:FromSnapshot})
//     → new branch ref creation (no snapshot copy; ref duplication only, O(1))
//  3. ForkOutput{Branch:NewBranch, Head:FromSnapshot} returned
//
// Core: Forking does not replicate snapshots.
// Only new refs are created (git branch meaning, invariant F1).
// Subsequently, SaveSession stacks a new snapshot on NewBranch with Parents=[FromSnapshot], forming a DAG branch.
type ForkSessionService struct {
	store outbound.SessionStore
}

// NewForkSessionService creates and injects dependencies for ForkSessionService.
func NewForkSessionService(store outbound.SessionStore) *ForkSessionService {
	return &ForkSessionService{store: store}
}

// Fork creates a new branch from the specified snapshot (ref duplication only, no snapshot copy).
func (s *ForkSessionService) Fork(ctx context.Context, in inbound.ForkInput) (inbound.ForkOutput, error) {
	if in.NewBranch == "" {
		return inbound.ForkOutput{}, domain.ErrNotFound
	}
	// Invariant F2 (git checkout -b meaning): Fails if the target branch already exists. PutRef is an upsert, so here it would silently move the existing branch head.
	if _, err := s.store.GetRef(ctx, in.RepoID, domain.RefBranch, in.NewBranch); err == nil {
		return inbound.ForkOutput{}, fmt.Errorf("%w: %q", domain.ErrBranchExists, in.NewBranch)
	}
	// Parent snapshot existence verification (invariant REF1/F1).
	if _, err := s.store.GetSnapshot(ctx, in.FromSnapshot); err != nil {
		return inbound.ForkOutput{}, err
	}
	if err := s.store.PutRef(ctx, domain.Ref{
		Kind:   domain.RefBranch,
		Name:   in.NewBranch,
		RepoID: in.RepoID,
		Target: in.FromSnapshot,
	}); err != nil {
		return inbound.ForkOutput{}, err
	}
	return inbound.ForkOutput{Branch: in.NewBranch, Head: in.FromSnapshot}, nil
}

// Ensure ForkSessionService implements inbound.ForkSession.
var _ inbound.ForkSession = (*ForkSessionService)(nil)
