package backendclient

import (
	"context"
	"errors"
	"fmt"
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

func anchoredSessionPath(c *BackendClient, repoID string, provider domain.ProviderKind, sessionID string) (string, error) {
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return "", err
	}
	if (provider != domain.ProviderClaude && provider != domain.ProviderCodex) || !domain.ValidAnchoredSessionID(sessionID) {
		return "", fmt.Errorf("%w: a session is named by its tool and its identifier", domain.ErrInvalidRef)
	}
	return c.reposPath(repoID) + "/anchored-sessions/" + url.PathEscape(string(provider)) + "/" + url.PathEscape(sessionID), nil
}

// ListRemovedAnchoredSessions lists the caller's own removed sessions with
// GET /repos/{repoID}/anchored-removals. A server from before removals has no
// such route and answers 404, which is read as no removal.
func (c *BackendClient) ListRemovedAnchoredSessions(ctx context.Context, repoID string) ([]domain.AnchoredSessionRemoval, error) {
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return nil, err
	}
	removals := []domain.AnchoredSessionRemoval{}
	err := c.doLimited(ctx, http.MethodGet, c.reposPath(repoID)+"/anchored-removals", nil, &removals, 4<<20)
	var status interface{ StatusCode() int }
	if errors.As(err, &status) && status.StatusCode() == http.StatusNotFound {
		return []domain.AnchoredSessionRemoval{}, nil
	}
	if err != nil {
		return nil, err
	}
	return removals, nil
}

// RemoveAnchoredSession removes a session, and the sessions it spawned, with
// DELETE /repos/{repoID}/anchored-sessions/{provider}/{sessionID}.
func (c *BackendClient) RemoveAnchoredSession(ctx context.Context, repoID string, provider domain.ProviderKind, sessionID string) ([]domain.AnchoredSessionRemoval, error) {
	path, err := anchoredSessionPath(c, repoID, provider, sessionID)
	if err != nil {
		return nil, err
	}
	var receipt struct {
		Removed []domain.AnchoredSessionRemoval `json:"removed"`
	}
	if err := c.doLimited(ctx, http.MethodDelete, path, nil, &receipt, 4<<20); err != nil {
		return nil, err
	}
	return receipt.Removed, nil
}

// AllowAnchoredSession takes back the removal of a session with
// DELETE /repos/{repoID}/anchored-sessions/{provider}/{sessionID}/removal.
func (c *BackendClient) AllowAnchoredSession(ctx context.Context, repoID string, provider domain.ProviderKind, sessionID string) error {
	path, err := anchoredSessionPath(c, repoID, provider, sessionID)
	if err != nil {
		return err
	}
	return c.doLimited(ctx, http.MethodDelete, path+"/removal", nil, nil, 1<<10)
}
