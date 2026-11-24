package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

// toolArgs is what a tool call may carry. A session is named by its tool and
// identifier, a part of it by its segment number or "rest", and a commit by its
// full number.
type toolArgs struct {
	Repository string `json:"repository"`
	Branch     string `json:"branch"`
	Limit      int    `json:"limit"`
	Events     int    `json:"events"`
	Query      string `json:"query"`
	Cursor     string `json:"cursor"`
	Provider   string `json:"provider"`
	Session    string `json:"session"`
	Segment    string `json:"segment"`
	Commit     string `json:"commit"`
}

func (s *Server) runTool(ctx context.Context, user domain.User, name string, raw json.RawMessage) (string, error) {
	// This adapter reads through the current server binary; a remote MCP
	// caller cannot select which document identities are read.
	identities := s.documentIdentities
	if len(identities) == 0 {
		identities = []domain.DocumentIdentity{domain.DocumentIdentityLegacy}
	}
	ctx = inbound.WithDocumentIdentities(ctx, identities)
	var args toolArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return "", fmt.Errorf("invalid arguments")
		}
	}
	switch name {
	case "repository_list":
		return s.repositoryPage(ctx, user, args)
	case "session_list", "session_read", "session_commit", "session_summaries", "session_search":
	default:
		return "", fmt.Errorf("unknown tool %q", name)
	}
	if strings.TrimSpace(args.Repository) == "" {
		return "", fmt.Errorf("repository is required; call repository_list first")
	}
	repo, err := s.resolveRepository(ctx, user, args.Repository)
	if err != nil {
		return "", err
	}
	return s.sessionTool(ctx, user, repo, name, args)
}

// Repository addresses are a server identity projection, not a derivation from
// the immutable historical connection URL.
type listedRepository struct {
	domain.Repo
	Path string
}

func repositoryPath(repo listedRepository) string { return repo.Path }

func (s *Server) canReadRepository(ctx context.Context, user domain.User, repo domain.Repo) (bool, error) {
	if repo.RepositoryID == "" {
		return false, nil
	}
	repository, err := s.identity.GetRepository(ctx, repo.RepositoryID)
	if err != nil {
		return false, err
	}
	if repository.IsPublic() {
		return true, nil
	}
	if role, ok := s.identity.RoleOf(ctx, repo.RepositoryID, user.ID); ok && role.AtLeast(domain.RoleViewer) {
		return true, nil
	}
	return s.identity.HasBreakGlassAccess(ctx, repo.RepositoryID, user.ID)
}

func (s *Server) visibleRepositories(ctx context.Context, user domain.User) ([]listedRepository, error) {
	repos, err := s.repositories.ListRepos(ctx, "default")
	if err != nil {
		return nil, err
	}
	visible := make([]listedRepository, 0, len(repos))
	for _, repo := range repos {
		allowed, accessErr := s.canReadRepository(ctx, user, repo)
		if accessErr != nil {
			return nil, accessErr
		}
		if allowed {
			metadata, err := s.identity.GetRepository(ctx, repo.RepositoryID)
			if err != nil {
				return nil, err
			}
			visible = append(visible, listedRepository{Repo: repo, Path: metadata.OwnerUsername + "/" + metadata.Slug})
		}
	}
	sort.Slice(visible, func(i, j int) bool { return repositoryPath(visible[i]) < repositoryPath(visible[j]) })
	return visible, nil
}

func (s *Server) resolveRepository(ctx context.Context, user domain.User, selector string) (domain.Repo, error) {
	selector = strings.Trim(strings.TrimSpace(selector), "/")
	if parsed, err := url.Parse(selector); err == nil && (parsed.Scheme == "https" || parsed.Scheme == "http") && parsed.Host != "" {
		selector = strings.Trim(parsed.Path, "/")
	}
	repos, err := s.visibleRepositories(ctx, user)
	if err != nil {
		return domain.Repo{}, fmt.Errorf("repository authorization unavailable")
	}
	for _, repo := range repos {
		if selector == string(repo.ID) || strings.EqualFold(selector, repositoryPath(repo)) {
			return repo.Repo, nil
		}
	}
	parts := strings.SplitN(selector, "/", 2)
	if len(parts) == 2 {
		metadata, err := s.identity.ReadableRepository(ctx, parts[0], parts[1], user.ID)
		if err == nil {
			for _, repo := range repos {
				if repo.RepositoryID == metadata.ID {
					return repo.Repo, nil
				}
			}
		}
	}
	return domain.Repo{}, fmt.Errorf("repository not found or not authorized")
}

func formatRepositoryList(repos []listedRepository, query string, limit int) (string, error) {
	query = strings.ToLower(strings.TrimSpace(query))
	if len([]rune(query)) > 128 {
		return "", fmt.Errorf("repository query must contain at most 128 characters")
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	filtered := make([]listedRepository, 0, min(len(repos), limit+1))
	for _, repo := range repos {
		if query == "" || strings.Contains(strings.ToLower(repositoryPath(repo)), query) {
			filtered = append(filtered, repo)
		}
	}
	if len(filtered) == 0 {
		return "No accessible repositories", nil
	}
	var b strings.Builder
	b.WriteString("Accessible CXTHub repositories:\n")
	for _, repo := range filtered[:min(len(filtered), limit)] {
		fmt.Fprintf(&b, "- %s · default branch %s · id %s\n", repositoryPath(repo), defaultBranch(repo.Repo), shortHash(repo.ID))
	}
	if omitted := len(filtered) - limit; omitted > 0 {
		fmt.Fprintf(&b, "… %d additional repositories omitted; narrow query or raise limit up to 100\n", omitted)
	}
	return b.String(), nil
}

func defaultBranch(repo domain.Repo) string {
	if strings.TrimSpace(repo.DefaultBranch) == "" {
		return "main"
	}
	return repo.DefaultBranch
}

func shortHash(hash domain.ContentHash) string {
	value := strings.TrimPrefix(string(hash), "sha256:")
	if len(value) > 10 {
		return value[:10]
	}
	return value
}
