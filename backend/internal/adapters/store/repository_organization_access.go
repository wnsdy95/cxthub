package store

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type repositoryOrganizationReader interface {
	outbound.RepositoryStore
	outbound.OrganizationStore
}

func readRepositoryOrganizationAccess(ctx context.Context, st repositoryOrganizationReader, id, actor string) (domain.OrganizationRepositoryAccess, error) {
	empty := domain.OrganizationRepositoryAccess{}
	if actor == "" {
		return empty, nil
	}
	repository, err := st.GetRepository(ctx, id)
	if err != nil || repository.OwnerNamespaceID == "" {
		return empty, err
	}
	namespace, err := st.GetNamespace(ctx, repository.OwnerNamespaceID)
	if err != nil || namespace.Kind != domain.NamespaceOrganization {
		return empty, err
	}
	organization, err := st.GetOrganization(ctx, namespace.OrganizationID)
	if err != nil {
		return empty, err
	}
	if organization.NamespaceID != namespace.ID {
		return empty, domain.ErrValidation
	}
	member, err := st.GetOrganizationMembership(ctx, organization.ID, actor)
	if errors.Is(err, domain.ErrNotFound) {
		return empty, nil
	}
	if err != nil {
		return empty, err
	}
	return domain.OrganizationRepositoryAccess{
		RepositoryID: id, OrganizationNamespaceID: namespace.ID, UserID: member.UserID, Role: member.Role,
	}, nil
}

func (s *FSStore) RepositoryOrganizationAccess(ctx context.Context, id, actor string) (domain.OrganizationRepositoryAccess, error) {
	return readRepositoryOrganizationAccess(ctx, s, id, actor)
}
