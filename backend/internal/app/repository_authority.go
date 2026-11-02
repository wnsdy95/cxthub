package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type RepositoryAuthorityProfile string

const (
	RepositoryAuthorityCompatibility RepositoryAuthorityProfile = "compatibility"
	RepositoryAuthorityComplete      RepositoryAuthorityProfile = "complete"
)

type RepositoryAuthorityMetadata interface {
	GetRepo(context.Context, domain.ContentHash) (domain.Repo, error)
}

type RepositoryAuthorityRepositories interface {
	GetRepository(context.Context, string) (domain.Repository, error)
	ListMembers(context.Context, string) ([]domain.Membership, error)
}

type RepositoryAuthorityTeams interface {
	RepositoryTeamAccess(context.Context, string, string) ([]domain.TeamRepositoryAccess, error)
}

type RepositoryAuthorityOrganizations interface {
	RepositoryOrganizationAccess(context.Context, string, string) (domain.OrganizationRepositoryAccess, error)
}

type RepositoryAuthorityAudit interface {
	GetNamespace(context.Context, string) (domain.Namespace, error)
	AppendOrganizationAudit(context.Context, domain.OrganizationAuditEvent) error
}

type RepositoryAuthorityConfig struct {
	Profile       RepositoryAuthorityProfile
	Metadata      RepositoryAuthorityMetadata
	Repositories  RepositoryAuthorityRepositories
	Teams         RepositoryAuthorityTeams
	Organizations RepositoryAuthorityOrganizations
	Locker        outbound.RepositoryAccessLocker
	Audit         RepositoryAuthorityAudit
	Transactional bool
}

type RepositoryAuthority struct {
	metadata      RepositoryAuthorityMetadata
	repositories  RepositoryAuthorityRepositories
	teams         RepositoryAuthorityTeams
	organizations RepositoryAuthorityOrganizations
	locker        outbound.RepositoryAccessLocker
	audit         RepositoryAuthorityAudit
	transactional bool
}

func NewRepositoryAuthority(config RepositoryAuthorityConfig) (*RepositoryAuthority, error) {
	if config.Profile != RepositoryAuthorityCompatibility && config.Profile != RepositoryAuthorityComplete {
		return nil, domain.ErrValidation
	}
	for _, dependency := range []any{config.Metadata, config.Repositories, config.Teams, config.Organizations, config.Locker, config.Audit} {
		if dependency != nil && !rootAdapterPresent(dependency) {
			return nil, domain.ErrValidation
		}
	}
	if config.Profile == RepositoryAuthorityComplete {
		for _, required := range []any{config.Metadata, config.Repositories, config.Teams, config.Organizations, config.Audit} {
			if required == nil {
				return nil, domain.ErrCatalogUnsupported
			}
		}
		if config.Transactional && config.Locker == nil {
			return nil, domain.ErrCatalogUnsupported
		}
	}
	return &RepositoryAuthority{metadata: config.Metadata, repositories: config.Repositories,
		teams: config.Teams, organizations: config.Organizations, locker: config.Locker,
		audit: config.Audit, transactional: config.Transactional}, nil
}
