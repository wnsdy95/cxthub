package app

import (
	"context"

	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// ListSessionsService implements the ListSessions inbound port as a use-case service.
//
// Dependencies:
// - SessionStore.
//
// Processing flow:
//  1. SessionStore.ListSnapshots(repoID, branch) → snapshot list
//  2. SessionStore.ListRefs(repoID) → ref list
//  3. Return ListOutput{Snapshots, Refs}
type ListSessionsService struct {
	store outbound.SessionStore
}

// NewListSessionsService creates and injects dependencies for ListSessionsService.
func NewListSessionsService(store outbound.SessionStore) *ListSessionsService {
	return &ListSessionsService{store: store}
}

// List retrieves the snapshot and ref lists for a repo/branch.
func (s *ListSessionsService) List(ctx context.Context, in inbound.ListInput) (inbound.ListOutput, error) {
	snaps, err := s.store.ListSnapshots(ctx, in.RepoID, in.Branch)
	if err != nil {
		return inbound.ListOutput{}, err
	}
	refs, err := s.store.ListRefs(ctx, in.RepoID)
	if err != nil {
		return inbound.ListOutput{}, err
	}
	return inbound.ListOutput{Snapshots: snaps, Refs: refs}, nil
}

// Ensure ListSessionsService implements inbound.ListSessions.
var _ inbound.ListSessions = (*ListSessionsService)(nil)
