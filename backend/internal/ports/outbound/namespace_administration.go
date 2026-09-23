package outbound

import "context"

// Called within IdentityTransactions; identities and previous URLs are retained.
type NamespaceAdministration interface {
	RenameOrganizationNamespace(context.Context, string, string) error
	RenameEnterpriseSlug(context.Context, string, string) error
}
