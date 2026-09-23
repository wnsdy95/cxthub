package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// TransferRepositoryNamespace requires authority over both spaces. Existing
// direct collaborators stay; source-organization inheritance and team grants do
// not travel. Content IDs, bound sync IDs, and historical URL aliases stay fixed.
func (s *IdentityService) TransferRepositoryNamespace(ctx context.Context, actor, id, expectedNamespace, expectedSlug, destination string) (domain.Repository, error) {
	return identityResult(ctx, s, func(ctx context.Context) (domain.Repository, error) {
		repo, err := s.repositories.GetRepository(ctx, id)
		if err != nil {
			return domain.Repository{}, err
		}
		if repo.OwnerNamespaceID != expectedNamespace || repo.Slug != expectedSlug {
			return domain.Repository{}, domain.ErrConflict
		}
		if s.organization == nil {
			return domain.Repository{}, domain.ErrForbidden
		}
		old, err := s.organization.GetNamespace(ctx, repo.OwnerNamespaceID)
		if err != nil {
			return domain.Repository{}, err
		}
		if old.Kind == domain.NamespaceOrganization {
			if role, _ := s.OrganizationRoleOf(ctx, old.OrganizationID, actor); role != domain.OrganizationOwner {
				return domain.Repository{}, domain.ErrForbidden
			}
		} else if old.UserID != actor {
			return domain.Repository{}, domain.ErrForbidden
		}
		next, err := s.organization.GetNamespaceBySlug(ctx, strings.ToLower(strings.TrimSpace(destination)))
		if err != nil {
			return domain.Repository{}, err
		}
		if next.ID == old.ID {
			return repo, nil
		}
		if next.Kind == domain.NamespaceOrganization {
			if role, _ := s.OrganizationRoleOf(ctx, next.OrganizationID, actor); role != domain.OrganizationOwner {
				return domain.Repository{}, domain.ErrForbidden
			}
			policy, err := s.effectiveOrganizationPolicy(ctx, next.OrganizationID)
			if err != nil {
				return domain.Repository{}, err
			}
			if repo.IsPublic() && !policy.AllowPublicRepositories {
				return domain.Repository{}, domain.ErrConflict
			}
		} else if next.UserID != actor {
			return domain.Repository{}, domain.ErrForbidden
		}
		existing, err := s.repositories.GetRepositoryByNamespacePath(ctx, next.ID, repo.Slug)
		if err == nil && existing.ID != id {
			return domain.Repository{}, domain.ErrConflict
		}
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return domain.Repository{}, err
		}
		repo.OwnerNamespaceID, repo.OwnerUsername = next.ID, next.Slug
		if next.Kind == domain.NamespaceUser {
			repo.OwnerID = actor
		}
		if err = s.repositories.CreateRepository(ctx, repo); err != nil {
			return domain.Repository{}, err
		}
		// Organization ownership stays inherited. A transfer must not convert
		// the operator's temporary organization role into a direct grant.
		if next.Kind == domain.NamespaceUser {
			if err = s.repositories.AddMember(ctx, domain.Membership{RepositoryID: id, UserID: actor, Role: domain.RoleOwner, CreatedAt: time.Now().UTC()}); err != nil {
				return domain.Repository{}, err
			}
		}
		if old.Kind == domain.NamespaceOrganization {
			if s.teams == nil {
				return domain.Repository{}, domain.ErrForbidden
			}
			teams, err := s.teams.ListTeams(ctx, old.OrganizationID)
			if err != nil {
				return domain.Repository{}, err
			}
			for _, team := range teams {
				if err = s.teams.RemoveTeamRepositoryGrant(ctx, team.ID, id); err != nil {
					return domain.Repository{}, err
				}
			}
		}
		for _, ns := range []domain.Namespace{old, next} {
			if ns.Kind == domain.NamespaceOrganization {
				if err = s.organization.AppendOrganizationAudit(ctx, organizationAudit(ctx, ns.OrganizationID, actor, "repository.transferred", "repository", id, old.Slug+" -> "+next.Slug, time.Now().UTC())); err != nil {
					return domain.Repository{}, err
				}
			}
		}
		return repo, nil
	})
}
