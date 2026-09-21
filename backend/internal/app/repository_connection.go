package app

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type RepositoryConnection struct {
	RepositoryID  string             `json:"repository_id"`
	RepoID        domain.ContentHash `json:"repo_id"`
	RemoteURL     string             `json:"remote_url"`
	CanonicalPath string             `json:"canonical_path"`
}

// ResolveRepositoryConnection resolves display addresses before a client derives
// its content identity. Existing connections retain the original URL and hash.
func (s *IdentityService) ResolveRepositoryConnection(ctx context.Context, actor, raw string) (RepositoryConnection, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || u.RawQuery != "" || u.Fragment != "" {
		return RepositoryConnection{}, domain.ErrValidation
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || len(parts) > 3 {
		return RepositoryConnection{}, domain.ErrValidation
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return RepositoryConnection{}, domain.ErrValidation
		}
	}
	repository, err := s.repositoryByNamespacePath(ctx, parts[0], strings.Join(parts[1:], "/"))
	if err != nil {
		return RepositoryConnection{}, err
	}
	role, member := s.RoleOf(ctx, repository.ID, actor)
	if !member || !role.AtLeast(domain.RolePuller) {
		if !repository.IsPublic() || repository.PublicRole != "puller" {
			return RepositoryConnection{}, domain.ErrNotFound
		}
	}
	bindings, ok := s.repositories.(outbound.RepositoryBindings)
	if !ok {
		return RepositoryConnection{}, domain.ErrIntegrity
	}
	bound, err := bindings.GetBoundRepo(ctx, repository.ID)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return RepositoryConnection{}, err
	}
	path := "/" + repository.OwnerUsername + "/" + repository.Slug
	if errors.Is(err, domain.ErrNotFound) {
		if !member || !role.AtLeast(domain.RoleMember) {
			return RepositoryConnection{}, domain.ErrForbidden
		}
		u.Path = path
		u.RawPath = ""
		raw = u.String()
		bound = domain.Repo{RemoteURL: raw, ID: domain.HashContent([]byte(normalizeGitURL(raw)))}
	}
	if domain.HashContent([]byte(normalizeGitURL(bound.RemoteURL))) != bound.ID {
		return RepositoryConnection{}, domain.ErrIntegrity
	}
	return RepositoryConnection{RepositoryID: repository.ID, RepoID: bound.ID, RemoteURL: bound.RemoteURL, CanonicalPath: path}, nil
}
