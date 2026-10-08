package backendclient

import (
	"context"
	"net/http"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func supportsRootIdentity(identities []domain.DocumentIdentity) bool {
	seen := map[domain.DocumentIdentity]bool{}
	for _, identity := range identities {
		if identity.Validate() != nil || seen[identity] {
			return false
		}
		seen[identity] = true
	}
	return seen[domain.DocumentIdentityRootV1]
}

// Check compatibility, opt-in and admission before application side effects.
// Zero wants may use existing roots with new-root admission off. This is only
// a current observation; transfers still verify bytes and renegotiate freshly.
// It does not enable server publication or advertise a released CLI capability.
func (c *BackendClient) PreflightDocumentReferences(ctx context.Context, repo string, refs []domain.DocumentRef) error {
	var roots []domain.ContentHash
	seen := map[domain.ContentHash]domain.DocumentIdentity{}
	for _, ref := range refs {
		if err := ref.Validate(); err != nil {
			return err
		}
		if old, exists := seen[ref.Hash]; exists && old != ref.Identity {
			return domain.ErrHashMismatch
		}
		if _, exists := seen[ref.Hash]; !exists && ref.Identity == domain.DocumentIdentityRootV1 {
			roots = append(roots, ref.Hash)
		}
		seen[ref.Hash] = ref.Identity
	}
	if len(roots) == 0 {
		return ctx.Err()
	}
	if err := domain.ValidateContentHash(repo); err != nil {
		return err
	}
	// Both read-only requests must inspect the same destination and principal.
	endpoint, token := c.baseURL(), c.token()
	peer := c.frozenRequestClient(endpoint, token)
	var view struct {
		ID                     string                    `json:"id"`
		RequiredDocIdentity    domain.DocumentIdentity   `json:"required_doc_identity"`
		DocIdentitiesSupported []domain.DocumentIdentity `json:"doc_identities_supported"`
	}
	if err := peer.do(ctx, http.MethodGet, peer.reposPath(repo), nil, &view); err != nil {
		return err
	}
	if view.ID != repo {
		return domain.ErrHashMismatch
	}
	if !supportsRootIdentity(view.DocIdentitiesSupported) || view.RequiredDocIdentity != domain.DocumentIdentityRootV1 {
		return domain.ErrUnsupportedDocumentIdentity
	}
	var neg negotiateResp
	if err := peer.do(ctx, http.MethodPost, peer.reposPath(repo)+"/push/negotiate", negotiateReq{DocHaves: roots}, &neg); err != nil {
		return err
	}
	if len(neg.SnapshotWants) != 0 || len(neg.ChunkWants) != 0 {
		return domain.ErrHashMismatch
	}
	if err := validateNegotiatedSubset(roots, neg.DocWants); err != nil {
		return err
	}
	if !supportsRootIdentity(neg.DocIdentitiesSupported) || !neg.AsyncDocsSupported || !neg.ChunksSupported || !neg.BoundedChunksSupported || !containsString(neg.ChunkFormatsSupported, domain.ConversationManifestChunkFormat) {
		return domain.ErrUnsupportedDocumentIdentity
	}
	if len(neg.DocWants) != 0 && !neg.RootPublicationEnabled {
		return domain.ErrUnsupportedDocumentIdentity
	}
	return ctx.Err()
}
