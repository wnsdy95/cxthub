package backendclient

import (
	"context"
	"net/http"
	"net/url"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// PutAnchoredSession stores one session's report with
// PUT /repos/{repoID}/anchored-sessions/{provider}/{sessionID}. The server
// takes the uploader from the signed-in account and replaces the session's
// earlier report as a whole.
func (c *BackendClient) PutAnchoredSession(ctx context.Context, repoID string, upload domain.AnchoredSessionUpload) (domain.AnchoredSessionReceipt, error) {
	var receipt domain.AnchoredSessionReceipt
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return receipt, err
	}
	path := c.reposPath(repoID) + "/anchored-sessions/" + url.PathEscape(string(upload.Provider)) + "/" + url.PathEscape(upload.SessionID)
	if err := c.doLimited(ctx, http.MethodPut, path, upload.Body, &receipt, 1<<20); err != nil {
		return domain.AnchoredSessionReceipt{}, err
	}
	return receipt, nil
}
