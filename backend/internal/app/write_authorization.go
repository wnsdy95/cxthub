package app

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type writeActionKey struct{}

func writeAction(ctx context.Context, action string) context.Context {
	return context.WithValue(ctx, writeActionKey{}, action)
}

func (s *Service) authorizeRepositoryWrite(ctx context.Context, repoID domain.ContentHash) error {
	actor, system := inbound.RepositoryActor(ctx)
	if system {
		return nil
	}
	if actor == "" {
		return domain.ErrUnauthorized
	}
	// Registration resolves and locks the new repository binding in ensureRepo.
	if action, _ := ctx.Value(writeActionKey{}).(string); action == "register" {
		return nil
	}
	if s.repositories == nil {
		return domain.ErrForbidden
	}
	repo, err := s.meta.GetRepo(ctx, repoID)
	if err != nil {
		return err
	}
	if repo.RepositoryID == "" {
		return domain.ErrForbidden
	}
	if _, ok := s.meta.(outbound.RepositoryTransactions); ok {
		lock, ok := s.repositories.(outbound.RepositoryAccessLocker)
		if !ok {
			return domain.ErrForbidden
		}
		if err := lock.LockRepositoryAccess(ctx, repo.RepositoryID, actor); err != nil {
			return err
		}
	}
	record, err := s.repositories.GetRepository(ctx, repo.RepositoryID)
	if err != nil {
		return err
	}
	role, ok, err := repositoryRoleFor(ctx, s.repositories, record, actor)
	if err != nil {
		return err
	}
	if !ok || record.Archived || !role.AtLeast(domain.RoleMember) {
		return domain.ErrForbidden
	}
	action, _ := ctx.Value(writeActionKey{}).(string)
	switch action {
	case "": // Ordinary context writes require the member role checked above.
	case "manage":
		if !role.AtLeast(domain.RoleMaintainer) {
			return domain.ErrForbidden
		}
	case "settings":
		if !role.AtLeast(domain.RoleMaintainer) || !domain.PolicyAllows(record.SettingsPolicy, role == domain.RoleOwner) {
			return domain.ErrForbidden
		}
	default:
		return domain.ErrForbidden
	}
	return nil
}
