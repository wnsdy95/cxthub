package domain

// OrganizationRepositoryAccess contains current ownership and membership facts.
// It is not a persisted repository grant: demotion must revoke inheritance.
type OrganizationRepositoryAccess struct {
	RepositoryID            string
	OrganizationNamespaceID string
	UserID                  string
	Role                    OrganizationRole
}

func (a OrganizationRepositoryAccess) IsOwnerOf(repository Repository, actor string) bool {
	return actor != "" && a.UserID == actor && a.RepositoryID == repository.ID &&
		a.OrganizationNamespaceID != "" && a.OrganizationNamespaceID == repository.OwnerNamespaceID &&
		a.Role == OrganizationOwner
}

func CanTransferRepository(repository Repository, actor string, organization OrganizationRepositoryAccess) bool {
	return actor != "" && (repository.OwnerID == actor || organization.IsOwnerOf(repository, actor))
}
