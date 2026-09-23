package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

// Called after repository authorization, while the same repository/identity
// transaction still holds its locks. HTTP preflight is not an ownership fence.
func (s *Service) authorizePendingWrite(ctx context.Context, repo domain.ContentHash, session string) (domain.User, error) {
	actor, system := inbound.RepositoryActor(ctx)
	if system {
		return domain.User{}, nil
	}
	if actor == "" || s.repositories == nil {
		return domain.User{}, domain.ErrUnauthorized
	}
	user, err := s.repositories.GetUser(ctx, actor)
	if err != nil {
		return domain.User{}, err
	}
	pendings, err := s.meta.ListPendings(ctx, repo)
	if err != nil {
		return domain.User{}, err
	}
	for _, p := range pendings {
		if p.SessionID != session {
			continue
		}
		if user.Email != "" && p.Author.Email == user.Email {
			return user, nil
		}
		bound, err := s.meta.GetRepo(ctx, repo)
		if err != nil {
			return domain.User{}, err
		}
		record, err := s.repositories.GetRepository(ctx, bound.RepositoryID)
		if err != nil {
			return domain.User{}, err
		}
		role, ok, err := repositoryRoleFor(ctx, s.repositories, record, actor)
		if err != nil {
			return domain.User{}, err
		}
		if ok && role.AtLeast(domain.RoleMaintainer) {
			return user, nil
		}
		return domain.User{}, domain.ErrForbidden
	}
	return user, nil
}

// Unsync keys use the authenticated username (the existing wire contract),
// whereas RepositoryActor carries the stable account ID.
func (s *Service) authorizeUnsyncWrite(ctx context.Context, username string) error {
	actor, system := inbound.RepositoryActor(ctx)
	if system {
		return nil
	}
	if actor == "" || s.repositories == nil {
		return domain.ErrUnauthorized
	}
	user, err := s.repositories.GetUser(ctx, actor)
	if err != nil {
		return err
	}
	if username == "" || user.Username != username {
		return domain.ErrForbidden
	}
	return nil
}
