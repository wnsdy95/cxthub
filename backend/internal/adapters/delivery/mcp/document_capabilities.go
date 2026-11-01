package mcp

import "github.com/wnsdy95/cxthub/backend/internal/domain"

func (server *Server) SetDocumentIdentities(identities []domain.DocumentIdentity) error {
	seen := make(map[domain.DocumentIdentity]bool, len(identities))
	for _, identity := range identities {
		if err := identity.Validate(); err != nil {
			return err
		}
		if seen[identity] {
			return domain.ErrValidation
		}
		seen[identity] = true
	}
	if !seen[domain.DocumentIdentityLegacy] {
		return domain.ErrValidation
	}
	server.documentIdentities = append([]domain.DocumentIdentity(nil), identities...)
	return nil
}
