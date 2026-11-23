//go:build postgres

package app

import (
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var (
	_ RepositoryAuthorityMetadata      = (*store.PostgresStore)(nil)
	_ RepositoryAuthorityRepositories  = (*store.PostgresStore)(nil)
	_ RepositoryAuthorityTeams         = (*store.PostgresStore)(nil)
	_ RepositoryAuthorityOrganizations = (*store.PostgresStore)(nil)
	_ RepositoryAuthorityAudit         = (*store.PostgresStore)(nil)
	_ outbound.RepositoryAccessLocker  = (*store.PostgresStore)(nil)
)
