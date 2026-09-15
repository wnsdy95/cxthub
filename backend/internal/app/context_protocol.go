package app

import (
	"context"
	"errors"
	"fmt"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (s *Service) EnableContextProtocol(ctx context.Context, repo domain.ContentHash) error {
	if err := domain.ValidateContentHash(repo); err != nil {
		return err
	}
	if _, err := s.meta.GetRepo(ctx, repo); err != nil {
		return err
	}
	store, ok := s.meta.(interface {
		EnableContextProtocol(context.Context, domain.ContentHash) error
	})
	if !ok {
		return fmt.Errorf("context protocol storage unavailable")
	}
	return store.EnableContextProtocol(ctx, repo)
}

func (s *Service) checkContextWrite(ctx context.Context, repoID domain.ContentHash, next domain.Ref) error {
	repo, err := s.meta.GetRepo(ctx, repoID)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if repo.ContextProtocol == 0 {
		return nil
	}
	events, err := s.ListHistory(ctx, repoID)
	if err != nil {
		return err
	}
	var current *domain.Ref
	ref, err := s.meta.GetRef(ctx, repoID, next.Kind, next.Name)
	if err == nil {
		current = &ref
	} else if !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	return domain.ValidateContextRefWrite(repo.ContextProtocol, events, current, next)
}
