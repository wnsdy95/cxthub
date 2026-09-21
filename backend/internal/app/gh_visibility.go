package app

import (
	"context"
	"net/http"
	neturl "net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// GitHub public status sync — GHVisibilitySync determines the public status of a repository based on the linked GitHub repos (one-way: GitHub → cxthub).
//
// Rule (conservative): Only public when all linked GitHub repos are public (1 or more).
//   - Unauthenticated GitHub API: 200 = public, 404 = private or non-existent → private.
//   - Network failure or rate limit falls back to private (safe direction).
//   - Non-GitHub remotes (self-hosted, etc.) are excluded from the determination.

// ghAPIBase is the GitHub API base. For testing, inject a stub server using CXT_GH_API_BASE.
func ghAPIBase() string {
	if v := os.Getenv("CXT_GH_API_BASE"); v != "" {
		return strings.TrimSuffix(v, "/")
	}
	return "https://api.github.com"
}

// githubRepoPath extracts the "owner/repo" path from the git remote URL. Returns "" for non-GitHub URLs.
// githubRepoPath recognizes various GitHub remote formats including scp (git@github.com:o/r) and URL (ssh/git/http/https, with user info and port).
// It rejects paths with only '.' or '..' segments (path manipulation or abnormal).
// Non-GitHub remotes are ignored in sync determination.
func githubRepoPath(remote string) string {
	r := strings.TrimSuffix(strings.TrimSpace(remote), ".git")
	var path string
	if strings.HasPrefix(r, "git@github.com:") {
		path = strings.TrimPrefix(r, "git@github.com:") // scp format (not a URL)
	} else if u, err := neturl.Parse(r); err == nil && strings.EqualFold(u.Hostname(), "github.com") {
		// ssh:// git:// http:// https:// — captures user info, port, and case-insensitive host.
		path = strings.TrimPrefix(u.Path, "/")
	} else {
		return ""
	}
	// Exactly two segments + '.'/'..' segments are rejected (path manipulation or abnormal).
	seg := strings.Split(path, "/")
	if len(seg) != 2 {
		return ""
	}
	for _, x := range seg {
		if x == "." || x == ".." || !ghSegRe.MatchString(x) {
			return "" // rejects path manipulation or abnormal segments
		}
	}
	return path
}

// ghSegRe is the GitHub owner/repo segment character rule (individual '.' or '..' segments are rejected below).
var ghSegRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// ghRepoPublic checks a repository without authentication; only HTTP 200 is treated as public.
func ghRepoPublic(ctx context.Context, path string) bool {
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "GET", ghAPIBase()+"/repos/"+path, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// SyncRepositoryVisibility updates repository visibility based on GitHub status.
// If GHVisibilitySync is off, returns ErrValidation (manual toggle is the source of truth).
func (s *Service) SyncRepositoryVisibility(ctx context.Context, repositoryID string) (domain.Repository, error) {
	if s.repositories == nil {
		return domain.Repository{}, domain.ErrNotFound
	}
	repositoryRecord, err := s.repositories.GetRepository(ctx, repositoryID)
	if err != nil {
		return domain.Repository{}, err
	}
	if !repositoryRecord.GHVisibilitySync {
		return domain.Repository{}, domain.ErrValidation
	}
	repos, err := s.meta.ListRepos(ctx, "default")
	if err != nil {
		return domain.Repository{}, err
	}
	var paths []string
	for _, r := range repos {
		if r.RepositoryID != repositoryID {
			continue
		}
		if p := githubRepoPath(r.GitRemoteURL); p != "" {
			paths = append(paths, p)
		}
	}
	vis := domain.VisibilityPrivate
	if len(paths) > 0 {
		allPublic := true
		for _, p := range paths {
			if !ghRepoPublic(ctx, p) {
				allPublic = false
				break
			}
		}
		if allPublic {
			vis = domain.VisibilityPublic
		}
	}
	identity := NewIdentityService(nil, s.repositories)
	return identityResult(ctx, identity, func(ctx context.Context) (domain.Repository, error) {
		current, err := s.repositories.GetRepository(ctx, repositoryID)
		if err != nil {
			return domain.Repository{}, err
		}
		if !current.GHVisibilitySync {
			return domain.Repository{}, domain.ErrConflict
		}
		if vis == domain.VisibilityPublic && current.OwnerNamespaceID != "" && identity.organization != nil {
			ns, err := identity.organization.GetNamespace(ctx, current.OwnerNamespaceID)
			if err != nil {
				return domain.Repository{}, err
			}
			if ns.Kind == domain.NamespaceOrganization {
				policy, err := identity.effectiveOrganizationPolicy(ctx, ns.OrganizationID)
				if err != nil {
					return domain.Repository{}, err
				}
				if !policy.AllowPublicRepositories {
					vis = domain.VisibilityPrivate
				}
			}
		}
		now := time.Now().UTC()
		current.Visibility = vis
		current.GHSyncedAt = &now
		if err := s.repositories.CreateRepository(ctx, current); err != nil {
			return domain.Repository{}, err
		}
		return current, nil
	})
}
