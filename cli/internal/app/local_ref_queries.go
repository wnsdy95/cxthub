package app

import (
	"context"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type LocalRefQueryService struct {
	gitCtx outbound.GitContext
	store  outbound.LocalRefReader
}

func NewLocalRefQueryService(gitCtx outbound.GitContext, store outbound.LocalRefReader) *LocalRefQueryService {
	return &LocalRefQueryService{gitCtx: gitCtx, store: store}
}

func (s *LocalRefQueryService) Refs(ctx context.Context, cwd string) ([]domain.Ref, error) {
	repo, err := s.gitCtx.CurrentRepo(ctx, cwd)
	if err != nil {
		return nil, err
	}
	return s.store.ListRefs(ctx, string(repo.ID))
}

func (s *LocalRefQueryService) Tags(ctx context.Context, cwd string) ([]domain.Ref, error) {
	refs, err := s.Refs(ctx, cwd)
	if err != nil {
		return nil, err
	}
	return visibleTags(refs)
}

func visibleTags(refs []domain.Ref) ([]domain.Ref, error) {
	var tags []domain.Ref
	for _, r := range refs {
		if r.Kind != domain.RefTag {
			continue
		}
		_, lifecycle, err := domain.ParseBranchLifecycleRef(r)
		if err != nil {
			return nil, err
		}
		if !lifecycle {
			tags = append(tags, r)
		}
	}
	return tags, nil
}

func (s *LocalRefQueryService) StashList(ctx context.Context, cwd string) ([]domain.StashEntry, error) {
	repo, err := s.gitCtx.CurrentRepo(ctx, cwd)
	if err != nil {
		return nil, err
	}
	return s.store.StashList(ctx, string(repo.ID))
}

var _ inbound.LocalRefQueries = (*LocalRefQueryService)(nil)
