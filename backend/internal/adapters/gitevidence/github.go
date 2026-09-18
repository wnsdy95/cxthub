// Package gitevidence reads immutable Git objects; application/domain code owns
// reversal policy. It never trusts commit titles as evidence of cancellation.
package gitevidence

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type GitHub struct {
	client *http.Client
	base   string
	token  func() string
}

func NewGitHub(token func() string) *GitHub {
	return &GitHub{client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, base: "https://api.github.com", token: token}
}

var _ outbound.GitEvidenceReader = (*GitHub)(nil)
var segment = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func repositoryPath(origin string) (string, error) {
	origin = strings.TrimSuffix(strings.TrimSpace(origin), ".git")
	var path string
	if strings.HasPrefix(origin, "git@github.com:") {
		path = strings.TrimPrefix(origin, "git@github.com:")
	} else {
		u, err := url.Parse(origin)
		if err != nil || !strings.EqualFold(u.Hostname(), "github.com") {
			return "", fmt.Errorf("%w: Git host does not support evidence verification", domain.ErrValidation)
		}
		if u.RawQuery != "" || u.Fragment != "" {
			return "", domain.ErrValidation
		}
		path = strings.TrimPrefix(u.Path, "/")
	}
	parts := strings.Split(path, "/")
	if len(parts) != 2 {
		return "", domain.ErrValidation
	}
	for _, p := range parts {
		if p == "." || p == ".." || !segment.MatchString(p) {
			return "", domain.ErrValidation
		}
	}
	return parts[0] + "/" + parts[1], nil
}
func (g *GitHub) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "cxthub-git-evidence")
	if g.token != nil {
		if token := g.token(); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Git evidence provider returned HTTP %d", resp.StatusCode)
	}
	// Refuse truncation; a clipped JSON response must never be interpreted as a
	// complete tree. Provider bodies/errors/tokens are not included in diagnostics.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
	if err != nil {
		return err
	}
	if len(raw) > 16<<20 {
		return fmt.Errorf("Git evidence response exceeds limit")
	}
	if err = json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("invalid Git evidence response: %w", err)
	}
	return nil
}

type commitObject struct {
	SHA  string `json:"sha"`
	Tree struct {
		SHA string `json:"sha"`
	} `json:"tree"`
	Parents []struct {
		SHA string `json:"sha"`
	} `json:"parents"`
}

func (g *GitHub) commit(ctx context.Context, repo, oid string) (commitObject, error) {
	var out commitObject
	if err := domain.ValidateGitOID(oid); err != nil {
		return out, err
	}
	if err := g.get(ctx, "/repos/"+repo+"/git/commits/"+oid, &out); err != nil {
		return out, err
	}
	if out.SHA != oid || out.Parents == nil {
		return out, domain.ErrIntegrity
	}
	if err := domain.ValidateGitOID(out.Tree.SHA); err != nil {
		return out, domain.ErrIntegrity
	}
	return out, nil
}
func (g *GitHub) tree(ctx context.Context, repo, oid string) (map[string]domain.GitEntry, bool, error) {
	var out struct {
		SHA       string `json:"sha"`
		Truncated *bool  `json:"truncated"`
		Tree      []struct {
			Path string `json:"path"`
			Mode string `json:"mode"`
			Type string `json:"type"`
			SHA  string `json:"sha"`
		} `json:"tree"`
	}
	if err := g.get(ctx, "/repos/"+repo+"/git/trees/"+oid+"?recursive=1", &out); err != nil {
		return nil, false, err
	}
	if out.SHA != oid || out.Truncated == nil || out.Tree == nil {
		return nil, false, domain.ErrIntegrity
	}
	if *out.Truncated {
		return nil, false, nil
	}
	entries := map[string]domain.GitEntry{}
	for _, item := range out.Tree {
		if err := domain.ValidateGitPath(item.Path); err != nil {
			return nil, false, domain.ErrIntegrity
		}
		if err := domain.ValidateGitOID(item.SHA); err != nil {
			return nil, false, domain.ErrIntegrity
		}
		switch item.Type {
		case "tree":
			if item.Mode != "040000" {
				return nil, false, domain.ErrIntegrity
			}
			continue
		case "blob":
			if item.Mode != "100644" && item.Mode != "100755" && item.Mode != "120000" {
				return nil, false, domain.ErrIntegrity
			}
		case "commit":
			if item.Mode != "160000" {
				return nil, false, domain.ErrIntegrity
			}
		default:
			return nil, false, domain.ErrIntegrity
		}
		if _, ok := entries[item.Path]; ok {
			return nil, false, domain.ErrIntegrity
		}
		entries[item.Path] = domain.GitEntry{OID: item.SHA, Mode: item.Mode}
	}
	return entries, true, nil
}
func (g *GitHub) ReadCommitDelta(ctx context.Context, origin, oid, parent string) (domain.GitCommitDelta, error) {
	out := domain.GitCommitDelta{Commit: oid, Changes: []domain.GitPathChange{}}
	repo, err := repositoryPath(origin)
	if err != nil {
		return out, err
	}
	commit, err := g.commit(ctx, repo, oid)
	if err != nil {
		return out, err
	}
	for _, p := range commit.Parents {
		out.Parents = append(out.Parents, p.SHA)
	}
	if parent == "" && len(out.Parents) == 1 {
		parent = out.Parents[0]
	}
	out.Parent = parent
	if err = out.Validate(); err != nil {
		return out, err
	}
	after, complete, err := g.tree(ctx, repo, commit.Tree.SHA)
	if err != nil || !complete {
		return out, err
	}
	before := map[string]domain.GitEntry{}
	if parent != "" {
		prior, err := g.commit(ctx, repo, parent)
		if err != nil {
			return out, err
		}
		before, complete, err = g.tree(ctx, repo, prior.Tree.SHA)
		if err != nil || !complete {
			return out, err
		}
	}
	for path, old := range before {
		current := after[path]
		if current != old {
			out.Changes = append(out.Changes, domain.GitPathChange{Path: path, Before: old, After: current})
		}
	}
	for path, current := range after {
		if _, ok := before[path]; !ok {
			out.Changes = append(out.Changes, domain.GitPathChange{Path: path, After: current})
		}
	}
	sort.Slice(out.Changes, func(i, j int) bool { return out.Changes[i].Path < out.Changes[j].Path })
	out.Complete = true
	return out, out.Validate()
}
func (g *GitHub) IsGitAncestor(ctx context.Context, origin, ancestor, descendant string) (bool, error) {
	if err := domain.ValidateGitOID(ancestor); err != nil {
		return false, err
	}
	if err := domain.ValidateGitOID(descendant); err != nil {
		return false, err
	}
	repo, err := repositoryPath(origin)
	if err != nil {
		return false, err
	}
	if ancestor == descendant {
		return true, nil
	}
	var out struct {
		Status string `json:"status"`
		Base   struct {
			SHA string `json:"sha"`
		} `json:"base_commit"`
		MergeBase struct {
			SHA string `json:"sha"`
		} `json:"merge_base_commit"`
	}
	if err = g.get(ctx, "/repos/"+repo+"/compare/"+ancestor+"..."+descendant+"?per_page=1", &out); err != nil {
		return false, err
	}
	if out.Base.SHA != ancestor {
		return false, domain.ErrIntegrity
	}
	switch out.Status {
	case "ahead", "identical":
		if out.MergeBase.SHA != ancestor {
			return false, domain.ErrIntegrity
		}
		return true, nil
	case "behind", "diverged":
		return false, nil
	default:
		return false, domain.ErrIntegrity
	}
}
