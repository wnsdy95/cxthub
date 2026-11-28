package http

import (
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// A runtime capability, never persisted as repository configuration. Old
// clients ignore it; new clients treat an absent/zero version as unsupported.
type repoPullView struct {
	DocIdentitiesSupported []domain.DocumentIdentity `json:"doc_identities_supported"`
	RootPublicationEnabled bool                      `json:"root_publication_enabled"`
	domain.Repo
}
