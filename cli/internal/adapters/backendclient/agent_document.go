package backendclient

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// FetchAgentDocument performs a fresh repository-authorized read. Package
// assembly cannot bypass revocation by serving a body from an unrelated cache.
func (c *BackendClient) FetchAgentDocument(ctx context.Context, repo string, hash domain.ContentHash) (domain.SessionDoc, error) {
	var doc domain.SessionDoc
	if domain.ValidateContentHash(domain.ContentHash(repo)) != nil || domain.ValidateContentHash(hash) != nil {
		return doc, domain.ErrHashMismatch
	}
	if err := c.doLimited(ctx, http.MethodGet, c.reposPath(repo)+"/docs/"+url.PathEscape(string(hash)), nil, &doc, 64<<20); err != nil {
		return doc, err
	}
	if doc.Hash != hash {
		return doc, domain.ErrHashMismatch
	}
	return doc, domain.ValidateSessionDocHash(doc)
}

// SyncRemoteIdentity is stable across token rotation and excludes userinfo,
// query parameters and fragments. No secret is persisted with observations.
func (c *BackendClient) SyncRemoteIdentity() string {
	u, err := url.Parse(c.baseURL())
	if err != nil || u.Host == "" {
		return ""
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	u.Scheme, u.Host = strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String()
}
