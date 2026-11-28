// Package gitevidence reads a repository's Git state from GitHub (branch heads,
// commits, pull requests and the default branch) for the remote Git reader.
// Application code decides what that state means.
package gitevidence

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const (
	scopedLimitMaxEntries  = 4096
	scopedLimitMaxKeyBytes = 8 << 20
)

type GitHub struct {
	rateMu           sync.Mutex
	credential       func(context.Context, string) (string, string, string, error)
	scopedLimits     map[string]time.Time
	scopedKeyBytes   int
	scopedOverflowAt time.Time
	resumeAt         time.Time
	client           *http.Client
	base             string
}

// NewGitHub reads public GitHub repositories without a credential. A server
// reads a private repository only through the installation it is bound to,
// with NewAuthenticated; no credential of the server stands in for one.
func NewGitHub() *GitHub {
	return &GitHub{client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, base: "https://api.github.com"}
}

// NewAuthenticated obtains repository-scoped credentials on every request.
func NewAuthenticated(resolve func(context.Context, string) (string, string, string, error)) *GitHub {
	g := NewGitHub()
	g.credential = resolve
	g.scopedLimits = map[string]time.Time{}
	return g
}

var segment = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func (provider *GitHub) sweepScopedLimits(now time.Time) {
	for bucket, until := range provider.scopedLimits {
		if !now.Before(until) {
			delete(provider.scopedLimits, bucket)
			provider.scopedKeyBytes -= len(bucket)
		}
	}
	if !now.Before(provider.scopedOverflowAt) {
		provider.scopedOverflowAt = time.Time{}
	}
}

func (provider *GitHub) rateLimited(bucket string, now time.Time) bool {
	provider.rateMu.Lock()
	defer provider.rateMu.Unlock()
	provider.sweepScopedLimits(now)
	if bucket == "" {
		return now.Before(provider.resumeAt)
	}
	return now.Before(provider.scopedLimits[bucket]) || now.Before(provider.scopedOverflowAt)
}

func (provider *GitHub) retainRateLimit(bucket string, resume, now time.Time) {
	provider.rateMu.Lock()
	defer provider.rateMu.Unlock()
	provider.sweepScopedLimits(now)
	if bucket == "" {
		if resume.After(provider.resumeAt) {
			provider.resumeAt = resume
		}
		return
	}
	if !now.Before(resume) {
		return
	}
	if until, exists := provider.scopedLimits[bucket]; exists {
		if resume.After(until) {
			provider.scopedLimits[strings.Clone(bucket)] = resume
		}
		return
	}
	if len(provider.scopedLimits) >= scopedLimitMaxEntries || len(bucket) > scopedLimitMaxKeyBytes-provider.scopedKeyBytes {
		if resume.After(provider.scopedOverflowAt) {
			provider.scopedOverflowAt = resume
		}
		return
	}
	if provider.scopedLimits == nil {
		provider.scopedLimits = make(map[string]time.Time)
	}
	provider.scopedLimits[strings.Clone(bucket)] = resume
	provider.scopedKeyBytes += len(bucket)
}

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
	if err := ctx.Err(); err != nil {
		return err
	}
	token, bucket, path, err := g.authorize(ctx, path)
	if err != nil {
		return err
	}
	return g.fetch(ctx, token, bucket, path, out)
}

// authorize returns the credential for a read of path, the rate-limit bucket
// it counts against, and the path to ask the provider for.
func (g *GitHub) authorize(ctx context.Context, path string) (token, bucket, resolved string, err error) {
	if g.credential != nil {
		return g.credential(ctx, path)
	}
	return "", "", path, nil
}

func (g *GitHub) fetch(ctx context.Context, token, bucket, path string, out any) error {
	if g.rateLimited(bucket, time.Now()) {
		return outbound.ErrGitProviderRateLimited
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "cxthub-git-evidence")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == 429 || (resp.StatusCode == 403 && (resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != "")) {
			resume := time.Now().Add(time.Minute)
			if seconds, e := strconv.ParseInt(resp.Header.Get("Retry-After"), 10, 64); e == nil && seconds > 0 {
				resume = time.Now().Add(time.Duration(min(seconds, 86400)) * time.Second)
			}
			if epoch, e := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); e == nil && time.Unix(epoch, 0).After(resume) {
				resume = time.Unix(epoch, 0)
			}
			g.retainRateLimit(bucket, resume, time.Now())
		}
		return outbound.GitProviderStatusError{Status: resp.StatusCode}
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

func (g *GitHub) ListGitHeads(ctx context.Context, origin string, page int) ([]outbound.GitHead, bool, error) {
	if page < 1 {
		return nil, false, domain.ErrValidation
	}
	repo, err := repositoryPath(origin)
	if err != nil {
		return nil, false, err
	}
	var branches []struct {
		Name   string `json:"name"`
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err = g.get(ctx, "/repos/"+repo+"/branches?per_page=100&page="+strconv.Itoa(page), &branches); err != nil {
		return nil, false, err
	}
	if branches == nil || len(branches) > 100 {
		return nil, false, domain.ErrIntegrity
	}
	out := []outbound.GitHead{}
	seen := map[string]bool{}
	for _, b := range branches {
		if b.Name == "" || seen[b.Name] || domain.ValidateGitOID(b.Commit.SHA) != nil {
			return nil, false, domain.ErrIntegrity
		}
		seen[b.Name] = true
		out = append(out, outbound.GitHead{Ref: "refs/heads/" + b.Name, Commit: b.Commit.SHA})
	}
	return out, len(branches) == 100, nil
}
