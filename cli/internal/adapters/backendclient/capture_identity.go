package backendclient

import (
	"context"

	"net/http"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// ConfirmRootCapture confirms support for the supported prepared-root route.
// It neither enables repository policy nor uploads anything. The explicit CLI
// setting may remember success for LOCAL capture; every push still preflights.
func (c *BackendClient) ConfirmRootCapture(ctx context.Context, repo string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := domain.ValidateContentHash(repo); err != nil {
		return err
	}
	// Freeze a possibly lazy endpoint/credential for both requests.
	endpoint, token := c.baseURL(), c.token()
	peer := c.frozenRequestClient(endpoint, token)
	var view struct {
		ID         string                    `json:"id"`
		Required   domain.DocumentIdentity   `json:"required_doc_identity"`
		Identities []domain.DocumentIdentity `json:"doc_identities_supported"`
		Enabled    bool                      `json:"root_publication_enabled"`
	}
	if err := peer.doLimited(ctx, http.MethodGet, peer.reposPath(repo), nil, &view, 1<<20); err != nil {
		return err
	}
	if view.ID != repo {
		return domain.ErrHashMismatch
	}
	if view.Required != domain.DocumentIdentityRootV1 || !view.Enabled || !supportsRootIdentity(view.Identities) {
		return domain.ErrUnsupportedDocumentIdentity
	}
	var negotiated struct {
		Identities    []domain.DocumentIdentity `json:"doc_identities_supported"`
		Enabled       bool                      `json:"root_publication_enabled"`
		Async         bool                      `json:"async_docs_supported"`
		Chunks        bool                      `json:"chunks_supported"`
		Bounded       bool                      `json:"bounded_chunks_supported"`
		Formats       []string                  `json:"chunk_formats_supported"`
		SnapshotWants []domain.ContentHash      `json:"snapshot_wants"`
		DocWants      []domain.ContentHash      `json:"doc_wants"`
		ChunkWants    []domain.ContentHash      `json:"chunk_wants"`
	}
	empty := negotiateReq{SnapshotHaves: []domain.ContentHash{}, DocHaves: []domain.ContentHash{}}
	if err := peer.doLimited(ctx, http.MethodPost, peer.reposPath(repo)+"/push/negotiate", empty, &negotiated, 1<<20); err != nil {
		return err
	}
	if len(negotiated.SnapshotWants)+len(negotiated.DocWants)+len(negotiated.ChunkWants) != 0 {
		return domain.ErrHashMismatch
	}
	if !supportsRootIdentity(negotiated.Identities) || !negotiated.Enabled || !negotiated.Async || !negotiated.Chunks || !negotiated.Bounded || !containsString(negotiated.Formats, domain.ConversationManifestChunkFormat) {
		return domain.ErrUnsupportedDocumentIdentity
	}
	return ctx.Err()
}
