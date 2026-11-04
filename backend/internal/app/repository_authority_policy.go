package app

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type repositoryRoleSources struct {
	repositories  RepositoryAuthorityRepositories
	teams         RepositoryAuthorityTeams
	organizations RepositoryAuthorityOrganizations
}

func (sources repositoryRoleSources) roleFor(ctx context.Context, repository domain.Repository, actor string) (domain.MemberRole, bool, error) {
	if sources.repositories == nil {
		return "", false, domain.ErrForbidden
	}
	members, err := sources.repositories.ListMembers(ctx, repository.ID)
	if err != nil {
		return "", false, err
	}
	var grants []domain.TeamRepositoryAccess
	if sources.teams != nil {
		grants, err = sources.teams.RepositoryTeamAccess(ctx, repository.ID, actor)
		if err != nil {
			return "", false, err
		}
	}
	var organization domain.OrganizationRepositoryAccess
	if sources.organizations != nil {
		organization, err = sources.organizations.RepositoryOrganizationAccess(ctx, repository.ID, actor)
		if err != nil {
			return "", false, err
		}
	}
	role, exists := domain.EffectiveRepositoryRole(repository, members, actor, grants, organization)
	return role, exists, nil
}

func (authority *RepositoryAuthority) AuthorizeWrite(ctx context.Context, repo domain.ContentHash) error {
	if authority == nil {
		return domain.ErrValidation
	}
	actor, system := inbound.RepositoryActor(ctx)
	if system {
		return nil
	}
	if actor == "" {
		return domain.ErrUnauthorized
	}
	action, _ := ctx.Value(writeActionKey{}).(string)
	if action == "register" {
		return nil
	}
	record, role, exists, err := authority.loadAccess(ctx, repo, actor, domain.ErrForbidden)
	if err != nil {
		return err
	}
	if !domain.CanWriteRepositoryContext(record, role, exists, action) {
		return domain.ErrForbidden
	}
	return nil
}

func (authority *RepositoryAuthority) AuthorizeSecrets(ctx context.Context, repo domain.ContentHash, actor string) error {
	if authority == nil {
		return domain.ErrValidation
	}
	record, role, exists, err := authority.loadAccess(ctx, repo, actor, domain.ErrSecretsConsistency)
	if err != nil {
		return err
	}
	if !exists || !domain.CanEditSecrets(record, role) {
		return domain.ErrForbidden
	}
	return nil
}

func (authority *RepositoryAuthority) loadAccess(ctx context.Context, repo domain.ContentHash, actor string, missingLock error) (domain.Repository, domain.MemberRole, bool, error) {
	if authority.repositories == nil || authority.metadata == nil {
		return domain.Repository{}, "", false, domain.ErrForbidden
	}
	metadata, err := authority.metadata.GetRepo(ctx, repo)
	if err != nil {
		return domain.Repository{}, "", false, err
	}
	if metadata.RepositoryID == "" {
		return domain.Repository{}, "", false, domain.ErrForbidden
	}
	if authority.transactional {
		if authority.locker == nil {
			return domain.Repository{}, "", false, missingLock
		}
		if err := authority.locker.LockRepositoryAccess(ctx, metadata.RepositoryID, actor); err != nil {
			return domain.Repository{}, "", false, err
		}
	}
	record, err := authority.repositories.GetRepository(ctx, metadata.RepositoryID)
	if err != nil {
		return domain.Repository{}, "", false, err
	}
	role, exists, err := (repositoryRoleSources{repositories: authority.repositories, teams: authority.teams, organizations: authority.organizations}).roleFor(ctx, record, actor)
	return record, role, exists, err
}

func (authority *RepositoryAuthority) AuditWrite(ctx context.Context, repo domain.ContentHash) error {
	if authority == nil {
		return domain.ErrValidation
	}
	actor, system := inbound.RepositoryActor(ctx)
	if system {
		return nil
	}
	if authority.metadata == nil {
		return domain.ErrForbidden
	}
	metadata, err := authority.metadata.GetRepo(ctx, repo)
	if err != nil {
		return err
	}
	if metadata.RepositoryID == "" {
		return nil
	}
	action, _ := ctx.Value(auditOperationKey{}).(string)
	if action == "" {
		action = "context.updated"
	}
	return appendRepositoryAuditFromSources(ctx, authority.repositories, authority.audit, actor, metadata.RepositoryID, action)
}

func appendRepositoryAuditFromSources(ctx context.Context, repositories RepositoryAuthorityRepositories, audit RepositoryAuthorityAudit, actor, repositoryID, action string) error {
	if repositories == nil {
		return domain.ErrForbidden
	}
	record, err := repositories.GetRepository(ctx, repositoryID)
	if err != nil {
		return err
	}
	if record.OwnerNamespaceID == "" {
		return nil
	}
	if audit == nil {
		return domain.ErrForbidden
	}
	namespace, err := audit.GetNamespace(ctx, record.OwnerNamespaceID)
	if err != nil {
		return err
	}
	if namespace.Kind != domain.NamespaceOrganization {
		return nil
	}
	return audit.AppendOrganizationAudit(ctx, organizationAudit(ctx, namespace.OrganizationID, actor, action, "repository", repositoryID, "", time.Now().UTC()))
}
