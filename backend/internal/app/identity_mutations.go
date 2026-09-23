package app

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type identityMutationKey struct{}

var localIdentityMutation sync.Mutex

func (s *IdentityService) withIdentity(ctx context.Context, fn func(context.Context) error) error {
	if tx, ok := s.repositories.(outbound.IdentityTransactions); ok {
		return tx.WithinIdentity(ctx, fn)
	}
	// Development stores serialize commands in-process; only the production
	// transaction port promises rollback and cross-server atomicity.
	if ctx.Value(identityMutationKey{}) != nil {
		return fn(ctx)
	}
	localIdentityMutation.Lock()
	defer localIdentityMutation.Unlock()
	return fn(context.WithValue(ctx, identityMutationKey{}, true))
}
func identityResult[T any](ctx context.Context, s *IdentityService, fn func(context.Context) (T, error)) (out T, err error) {
	err = s.withIdentity(ctx, func(ctx context.Context) error { out, err = fn(ctx); return err })
	if err != nil {
		var zero T
		return zero, err
	}
	return out, nil
}

func repositoryRole(ctx context.Context, repositories outbound.RepositoryStore, id, actor string) (domain.MemberRole, bool) {
	if repositories == nil || actor == "" {
		return "", false
	}
	repository, err := repositories.GetRepository(ctx, id)
	if err != nil {
		return "", false
	}
	role, ok, _ := repositoryRoleFor(ctx, repositories, repository, actor)
	return role, ok
}

func repositoryOrganizationAccess(ctx context.Context, repositories outbound.RepositoryStore, id, actor string) (domain.OrganizationRepositoryAccess, error) {
	if reader, ok := repositories.(outbound.RepositoryOrganizationAccess); ok {
		return reader.RepositoryOrganizationAccess(ctx, id, actor)
	}
	return domain.OrganizationRepositoryAccess{}, nil
}

func repositoryRoleFor(ctx context.Context, repositories outbound.RepositoryStore, repository domain.Repository, actor string) (domain.MemberRole, bool, error) {
	members, err := repositories.ListMembers(ctx, repository.ID)
	if err != nil {
		return "", false, err
	}
	var grants []domain.TeamRepositoryAccess
	if teams, ok := repositories.(outbound.TeamStore); ok {
		grants, err = teams.RepositoryTeamAccess(ctx, repository.ID, actor)
		if err != nil {
			return "", false, err
		}
	}
	organization, err := repositoryOrganizationAccess(ctx, repositories, repository.ID, actor)
	if err != nil {
		return "", false, err
	}
	role, ok := domain.EffectiveRepositoryRole(repository, members, actor, grants, organization)
	return role, ok, nil
}

func (s *IdentityService) UpdateProfile(ctx context.Context, u domain.User, username, nickname, loadMode, avatar, locale *string) (domain.User, error) {
	return identityResult(ctx, s, func(ctx context.Context) (domain.User, error) {
		return s.mutateUpdateProfile(ctx, u, username, nickname, loadMode, avatar, locale)
	})
}

func (s *IdentityService) UpdateRepositorySettings(ctx context.Context, userID, repositoryID string, p RepositoryPatch) (domain.Repository, error) {
	return identityResult(ctx, s, func(ctx context.Context) (domain.Repository, error) {
		result, err := s.mutateUpdateRepositorySettings(ctx, userID, repositoryID, p)
		if err == nil {
			err = appendRepositoryAudit(ctx, s.repositories, userID, repositoryID, "repository.access_settings.updated")
		}
		return result, err
	})
}

func (s *IdentityService) TransferOwnership(ctx context.Context, actorID, repositoryID, targetID string) (domain.Repository, error) {
	return identityResult(ctx, s, func(ctx context.Context) (domain.Repository, error) {
		result, err := s.mutateTransferOwnership(ctx, actorID, repositoryID, targetID)
		if err == nil {
			err = appendRepositoryAudit(ctx, s.repositories, actorID, repositoryID, "repository.ownership.updated")
		}
		return result, err
	})
}

func (s *IdentityService) UpdateMemberRole(ctx context.Context, actorID, repositoryID, targetID string, role domain.MemberRole) error {
	return s.withIdentity(ctx, func(ctx context.Context) error {
		if err := s.mutateUpdateMemberRole(ctx, actorID, repositoryID, targetID, role); err != nil {
			return err
		}
		return appendRepositoryAudit(ctx, s.repositories, actorID, repositoryID, "repository.member.updated")
	})
}

func (s *IdentityService) RemoveMember(ctx context.Context, actorID, repositoryID, targetID string) error {
	return s.withIdentity(ctx, func(ctx context.Context) error {
		if err := s.mutateRemoveMember(ctx, actorID, repositoryID, targetID); err != nil {
			return err
		}
		return appendRepositoryAudit(ctx, s.repositories, actorID, repositoryID, "repository.member.removed")
	})
}

func (s *IdentityService) CreateRepository(ctx context.Context, owner domain.User, name string) (domain.Repository, error) {
	return identityResult(ctx, s, func(ctx context.Context) (domain.Repository, error) {
		return s.mutateCreateRepository(ctx, owner, name)
	})
}

func (s *IdentityService) Invite(ctx context.Context, userID, repositoryID, email string, role domain.MemberRole, ttl time.Duration) (domain.Invite, error) {
	return identityResult(ctx, s, func(ctx context.Context) (domain.Invite, error) {
		return s.mutateInvite(ctx, userID, repositoryID, email, role, ttl)
	})
}

func (s *IdentityService) RevokeInvite(ctx context.Context, userID, repositoryID, token string) error {
	return s.withIdentity(ctx, func(ctx context.Context) error { return s.mutateRevokeInvite(ctx, userID, repositoryID, token) })
}

func (s *IdentityService) CreateOrganization(ctx context.Context, creator domain.User, name, requestedSlug string) (domain.Organization, error) {
	return identityResult(ctx, s, func(ctx context.Context) (domain.Organization, error) {
		return s.mutateCreateOrganization(ctx, creator, name, requestedSlug)
	})
}

func (s *IdentityService) UpdateOrganizationProfile(ctx context.Context, actorID, organizationID string, name, logo *string) (domain.Organization, error) {
	return identityResult(ctx, s, func(ctx context.Context) (domain.Organization, error) {
		return s.mutateUpdateOrganizationProfile(ctx, actorID, organizationID, name, logo)
	})
}

func (s *IdentityService) UpdateOrganizationMember(ctx context.Context, actorID, organizationID, targetID string, role domain.OrganizationRole) error {
	return s.withIdentity(ctx, func(ctx context.Context) error {
		return s.mutateUpdateOrganizationMember(ctx, actorID, organizationID, targetID, role)
	})
}

func (s *IdentityService) RemoveOrganizationMember(ctx context.Context, actorID, organizationID, targetID string) error {
	return s.withIdentity(ctx, func(ctx context.Context) error {
		return s.mutateRemoveOrganizationMember(ctx, actorID, organizationID, targetID)
	})
}

func (s *IdentityService) UpdateOrganizationPolicy(ctx context.Context, actorID string, policy domain.OrganizationPolicy) (domain.OrganizationPolicy, error) {
	return identityResult(ctx, s, func(ctx context.Context) (domain.OrganizationPolicy, error) {
		return s.mutateUpdateOrganizationPolicy(ctx, actorID, policy)
	})
}

func (s *IdentityService) CreateOrganizationRepository(ctx context.Context, actor domain.User, organizationID, name string) (domain.Repository, error) {
	return identityResult(ctx, s, func(ctx context.Context) (domain.Repository, error) {
		return s.mutateCreateOrganizationRepository(ctx, actor, organizationID, name)
	})
}

func (s *IdentityService) CreateBreakGlassGrant(ctx context.Context, actorID, organizationID, repositoryID, reason string, minutes int) (domain.BreakGlassGrant, error) {
	return identityResult(ctx, s, func(ctx context.Context) (domain.BreakGlassGrant, error) {
		return s.mutateCreateBreakGlassGrant(ctx, actorID, organizationID, repositoryID, reason, minutes)
	})
}

// Administration accepts an explicit @handle as well as IDs returned by member
// lists. Resolution stays behind the caller's administration gate.
func (s *IdentityService) memberSubject(ctx context.Context, subject string) (string, error) {
	if strings.HasPrefix(subject, "@") {
		user, err := s.repositories.GetUserByUsername(ctx, strings.TrimPrefix(subject, "@"))
		return user.ID, err
	}
	user, err := s.repositories.GetUser(ctx, subject)
	return user.ID, err
}
