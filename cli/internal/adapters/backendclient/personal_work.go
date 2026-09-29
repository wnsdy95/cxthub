package backendclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// PersonalWorkPrincipal performs a fresh authenticated account read. Git author
// email, X-Cxt-Identity, cached preferences, and artifact prose are not identities.
// In particular, a development user's ID (dev:<email>) is not their email.
func (c *BackendClient) PersonalWorkPrincipal(ctx context.Context) (id, email string, err error) {
	if c == nil || c.token == nil {
		return "", "", fmt.Errorf("%w: --work-state requires an authenticated backend account; log in to the configured remote", domain.ErrAgentContextUnavailable)
	}
	token := c.token()
	if strings.TrimSpace(token) == "" {
		return "", "", fmt.Errorf("%w: --work-state requires an authenticated backend account; log in to the configured remote", domain.ErrAgentContextUnavailable)
	}
	var user struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}
	// Suppress the caller-controlled legacy identity header for this request.
	account := &BackendClient{baseURL: c.baseURL, token: func() string { return token }, httpc: c.httpc}
	if err := account.doLimited(ctx, http.MethodGet, "/me", nil, &user, 1<<20); err != nil {
		return "", "", fmt.Errorf("--work-state cannot authenticate the account through GET /me: %w", err)
	}
	if strings.TrimSpace(user.ID) == "" || strings.TrimSpace(user.Email) == "" {
		return "", "", fmt.Errorf("%w: GET /me returned no account ID or author email; personal handoff is unavailable on this backend", domain.ErrAgentContextUnavailable)
	}
	return user.ID, user.Email, nil
}

// FetchPersonalWorkDocument enforces the import's remaining cumulative byte
// budget on the HTTP body before decoding. Personal handoff must never use the
// much larger general archive reader or silently truncate a source document.
func (c *BackendClient) FetchPersonalWorkDocument(ctx context.Context, repo string, hash domain.ContentHash, remaining int64) (domain.SessionDoc, int64, error) {
	var doc domain.SessionDoc
	if remaining <= 0 || remaining > 8<<20 || domain.ValidateContentHash(domain.ContentHash(repo)) != nil || domain.ValidateContentHash(hash) != nil {
		return doc, 0, domain.ErrHashMismatch
	}
	path := c.reposPath(repo) + "/docs/" + url.PathEscape(string(hash))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL()+path, nil)
	if err != nil {
		return doc, 0, err
	}
	if token := c.token(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := c.httpc.Do(req)
	if err != nil {
		return doc, 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return doc, 0, newHTTPError(response.StatusCode, http.MethodGet, path, body)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, remaining+1))
	if err != nil {
		return doc, 0, err
	}
	if int64(len(raw)) > remaining {
		return doc, 0, fmt.Errorf("personal work document exceeds remaining %d-byte cumulative source budget", remaining)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return doc, 0, err
	}
	if doc.Hash != hash {
		return doc, 0, domain.ErrHashMismatch
	}
	if err := domain.ValidateSessionDocHash(doc); err != nil {
		return doc, 0, err
	}
	return doc, int64(len(raw)), nil
}
