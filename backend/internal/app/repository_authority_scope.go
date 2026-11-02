package app

import (
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func repositoryAuthorityFromStores(metadata RepositoryAuthorityMetadata, repositories outbound.RepositoryStore, transactional bool, profile RepositoryAuthorityProfile) (*RepositoryAuthority, error) {
	roles, err := repositoryRoleSourcesFromRepositories(repositories)
	if err != nil {
		return nil, err
	}
	locker, _ := repositories.(outbound.RepositoryAccessLocker)
	audit, _ := repositories.(outbound.OrganizationStore)
	return NewRepositoryAuthority(RepositoryAuthorityConfig{Profile: profile, Metadata: metadata,
		Repositories: repositories, Teams: roles.teams, Organizations: roles.organizations, Locker: locker,
		Audit: audit, Transactional: transactional})
}

func repositoryRoleSourcesFromRepositories(repositories outbound.RepositoryStore) (repositoryRoleSources, error) {
	teams, _ := repositories.(outbound.TeamStore)
	organizations, _ := repositories.(outbound.RepositoryOrganizationAccess)
	for _, dependency := range []any{repositories, teams, organizations} {
		if dependency != nil && !rootAdapterPresent(dependency) {
			return repositoryRoleSources{}, domain.ErrValidation
		}
	}
	return repositoryRoleSources{repositories: repositories, teams: teams, organizations: organizations}, nil
}

func (work *repositoryWork) validateCompleteAuthority() error {
	if work == nil {
		return domain.ErrCatalogUnsupported
	}
	if work.configurationError != nil {
		return work.configurationError
	}
	if work.authority == nil {
		return domain.ErrCatalogUnsupported
	}
	authority := work.authority
	_, err := NewRepositoryAuthority(RepositoryAuthorityConfig{Profile: RepositoryAuthorityComplete,
		Metadata: authority.metadata, Repositories: authority.repositories, Teams: authority.teams,
		Organizations: authority.organizations, Locker: authority.locker,
		Audit: authority.audit, Transactional: authority.transactional})
	return err
}
