package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

var _ inbound.StagingStash = (*StagingService)(nil)

func (s *StagingService) StashIndex(ctx context.Context, cwd string, expected domain.ContentHash) (domain.StagingStash, error) {
	store, ok := s.index.(outbound.StagingStashStore)
	if !ok {
		return domain.StagingStash{}, fmt.Errorf("frozen index stash unavailable")
	}
	repo, err := s.git.CurrentRepo(ctx, cwd)
	if err != nil {
		return domain.StagingStash{}, err
	}
	index, p, err := s.index.ReadStaging(ctx, repo.ID)
	if err != nil {
		return domain.StagingStash{}, err
	}
	if expected != "" && expected != index.Revision {
		return domain.StagingStash{}, domain.ErrSyncConflict
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return domain.StagingStash{}, err
	}
	return store.StashIndex(ctx, domain.StagingStash{Version: domain.StagingVersion, ID: hex.EncodeToString(id[:]), Index: index, Position: p, CreatedAt: time.Now().UTC()})
}
func (s *StagingService) PopIndex(ctx context.Context, cwd, id string, expected domain.ContentHash) (domain.StagingIndex, error) {
	store, ok := s.index.(outbound.StagingStashStore)
	if !ok {
		return domain.StagingIndex{}, fmt.Errorf("frozen index stash unavailable")
	}
	repo, index, p, err := s.selection(ctx, cwd)
	if err != nil {
		return index, err
	}
	if expected != "" && expected != index.Revision {
		return index, domain.ErrSyncConflict
	}
	return store.PopIndex(ctx, repo.ID, id, index.Revision, p)
}
func (s *StagingService) ListIndexStashes(ctx context.Context, cwd string) ([]domain.StagingStash, error) {
	store, ok := s.index.(outbound.StagingStashStore)
	if !ok {
		return nil, fmt.Errorf("frozen index stash unavailable")
	}
	repo, err := s.git.CurrentRepo(ctx, cwd)
	if err != nil {
		return nil, err
	}
	return store.ListIndexStashes(ctx, repo.ID)
}
