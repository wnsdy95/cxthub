package backendclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

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

// ListAnchoredSessions lists the sessions the caller may read with
// GET /repos/{repoID}/anchored-sessions.
func (c *BackendClient) ListAnchoredSessions(ctx context.Context, repoID string) ([]domain.AnchoredSessionListed, error) {
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return nil, err
	}
	sessions := []domain.AnchoredSessionListed{}
	if err := c.doLimited(ctx, http.MethodGet, c.reposPath(repoID)+"/anchored-sessions", nil, &sessions, 64<<20); err != nil {
		return nil, err
	}
	return sessions, nil
}

// ListAnchoredCommitSummaries lists the summaries at one commit with
// GET /repos/{repoID}/anchored-commits/{sha}/summaries.
func (c *BackendClient) ListAnchoredCommitSummaries(ctx context.Context, repoID, commit string) ([]domain.AnchoredSummary, error) {
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return nil, err
	}
	if !fullCommitNumber(commit) {
		return nil, fmt.Errorf("%w: a commit is named by its full number", domain.ErrInvalidRef)
	}
	summaries := []domain.AnchoredSummary{}
	if err := c.doLimited(ctx, http.MethodGet, c.reposPath(repoID)+"/anchored-commits/"+url.PathEscape(commit)+"/summaries", nil, &summaries, 16<<20); err != nil {
		return nil, err
	}
	return summaries, nil
}

// ReadRemoteGitSyncedAt reads when the server last finished reading the
// repository from GitHub, with GET /repos/{repoID}/remote-git. It is empty
// when the server never did.
func (c *BackendClient) ReadRemoteGitSyncedAt(ctx context.Context, repoID string) (string, error) {
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return "", err
	}
	var view struct {
		SyncedAt string `json:"synced_at"`
	}
	if err := c.doLimited(ctx, http.MethodGet, c.reposPath(repoID)+"/remote-git?limit=1", nil, &view, 64<<20); err != nil {
		return "", err
	}
	return view.SyncedAt, nil
}

// RequestRemoteGitRefresh asks the server to read the repository from GitHub
// again, with POST /repos/{repoID}/remote-git/refresh.
func (c *BackendClient) RequestRemoteGitRefresh(ctx context.Context, repoID string) error {
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return err
	}
	var answer struct {
		Requested bool `json:"requested"`
	}
	return c.doLimited(ctx, http.MethodPost, c.reposPath(repoID)+"/remote-git/refresh", struct{}{}, &answer, 1<<10)
}

// SetAnchoredSessionArchived archives a session or restores it with
// PUT /repos/{repoID}/anchored-sessions/{provider}/{sessionID}/archive.
func (c *BackendClient) SetAnchoredSessionArchived(ctx context.Context, repoID string, provider domain.ProviderKind, sessionID string, archived bool) error {
	path, err := anchoredSessionPath(c, repoID, provider, sessionID)
	if err != nil {
		return err
	}
	var answer struct {
		Archived bool `json:"archived"`
	}
	return c.doLimited(ctx, http.MethodPut, path+"/archive", struct {
		Archived bool `json:"archived"`
	}{archived}, &answer, 1<<10)
}

// GetAnchoredSessionBody reads how a session is cut, as far as the caller
// may read it, with GET /repos/{repoID}/anchored-sessions/{provider}/{sessionID}.
func (c *BackendClient) GetAnchoredSessionBody(ctx context.Context, repoID string, provider domain.ProviderKind, sessionID string) (domain.AnchoredSessionBody, error) {
	var session struct {
		Body domain.AnchoredSessionBody `json:"body"`
	}
	path, err := anchoredSessionPath(c, repoID, provider, sessionID)
	if err != nil {
		return session.Body, err
	}
	err = c.doLimited(ctx, http.MethodGet, path, nil, &session, 16<<20)
	return session.Body, err
}

// anchoredContentBudget is the size of one page of a conversation the CLI
// asks for, in bytes of event JSON: the most the server gives.
const anchoredContentBudget = 64 << 10

// ReadAnchoredSessionContent reads a page of a session's conversation as its
// events, from one event and one byte of it on, with
// GET /repos/{repoID}/anchored-sessions/{provider}/{sessionID}/content.
func (c *BackendClient) ReadAnchoredSessionContent(ctx context.Context, repoID string, provider domain.ProviderKind, sessionID string, index, offset int) (domain.AnchoredContentPage, error) {
	var page domain.AnchoredContentPage
	path, err := anchoredSessionPath(c, repoID, provider, sessionID)
	if err != nil {
		return page, err
	}
	if index < 0 || offset < 0 {
		return page, fmt.Errorf("%w: a conversation is read from an event and a byte of it", domain.ErrInvalidRef)
	}
	query := url.Values{"index": {strconv.Itoa(index)}, "offset": {strconv.Itoa(offset)}, "budget": {strconv.Itoa(anchoredContentBudget)}}
	err = c.doLimited(ctx, http.MethodGet, path+"/content?"+query.Encode(), nil, &page, 8<<20)
	return page, err
}
