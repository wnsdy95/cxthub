// Package githubapp contains GitHub's transport and credential handling only.
package githubapp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
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

type Config struct{ AppID, ClientID, ClientSecret, Slug, PrivateKey, CallbackURL string }
type tokenSlot struct {
	mu      sync.Mutex
	token   string
	expires time.Time
}
type Client struct {
	cfg      Config
	key      *rsa.PrivateKey
	http     *http.Client
	api, web string
	mu       sync.Mutex
	tokens   map[int64]*tokenSlot
	limits   map[string]time.Time
}

var _ outbound.GitHubApp = (*Client)(nil)

func New(cfg Config) (*Client, error) {
	if cfg.AppID == "" || cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.Slug == "" || cfg.CallbackURL == "" {
		return nil, errors.New("incomplete GitHub App configuration")
	}
	if strings.ContainsAny(cfg.Slug, "/?# ") {
		return nil, domain.ErrValidation
	}
	block, _ := pem.Decode([]byte(strings.ReplaceAll(cfg.PrivateKey, `\n`, "\n")))
	if block == nil {
		return nil, errors.New("invalid GitHub App private key")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		v, e := x509.ParsePKCS8PrivateKey(block.Bytes)
		if e != nil {
			return nil, errors.New("invalid GitHub App private key")
		}
		var ok bool
		key, ok = v.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("GitHub App key must be RSA")
		}
	}
	return &Client{cfg: cfg, key: key, http: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, api: "https://api.github.com", web: "https://github.com", tokens: map[int64]*tokenSlot{}, limits: map[string]time.Time{}}, nil
}
func (c *Client) InstallURL(state string) string {
	return c.web + "/apps/" + url.PathEscape(c.cfg.Slug) + "/installations/new?state=" + url.QueryEscape(state)
}
func (c *Client) AuthorizeURL(state, verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{"client_id": {c.cfg.ClientID}, "redirect_uri": {c.cfg.CallbackURL}, "state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}}
	return c.web + "/login/oauth/authorize?" + q.Encode()
}
func (c *Client) jwt() (string, error) {
	now := time.Now()
	payload, _ := json.Marshal(map[string]any{"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(8 * time.Minute).Unix(), "iss": c.cfg.ClientID})
	s := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + base64.RawURLEncoding.EncodeToString(payload)
	sum := sha256.Sum256([]byte(s))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, sum[:])
	return s + "." + base64.RawURLEncoding.EncodeToString(sig), err
}

// Errors never contain response bodies, credentials, or authorization codes.
func (c *Client) request(ctx context.Context, method, target, token, bucket string, body, out any) error {
	c.mu.Lock()
	blocked := time.Now().Before(c.limits[bucket])
	c.mu.Unlock()
	if blocked {
		return errors.New("GitHub request is rate limited; retry later")
	}
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(raw))
	if err != nil {
		return domain.ErrValidation
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "cxthub")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return errors.New("GitHub request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode == 429 || resp.StatusCode == 403 && (resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != "") {
		until := time.Now().Add(time.Minute)
		if n, e := strconv.Atoi(resp.Header.Get("Retry-After")); e == nil && n > 0 {
			until = time.Now().Add(time.Duration(min(n, 86400)) * time.Second)
		}
		if n, e := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); e == nil && time.Unix(n, 0).After(until) {
			until = time.Unix(n, 0)
		}
		c.mu.Lock()
		c.limits[bucket] = until
		c.mu.Unlock()
		return errors.New("GitHub request is rate limited; retry later")
	}
	if resp.StatusCode == 404 {
		return domain.ErrNotFound
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return domain.ErrForbidden
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("GitHub request returned HTTP %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
	if err != nil || len(data) > 16<<20 {
		return domain.ErrIntegrity
	}
	if json.Unmarshal(data, out) != nil {
		return domain.ErrIntegrity
	}
	return nil
}
func (c *Client) slot(id int64) *tokenSlot {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.tokens[id]
	if s == nil {
		s = &tokenSlot{}
		c.tokens[id] = s
	}
	return s
}
func (c *Client) Invalidate(id int64) { c.mu.Lock(); delete(c.tokens, id); c.mu.Unlock() }
func (c *Client) InstallationToken(ctx context.Context, id int64) (string, error) {
	if id <= 0 {
		return "", domain.ErrValidation
	}
	s := c.slot(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && time.Now().Add(time.Minute).Before(s.expires) {
		return s.token, nil
	}
	jwt, err := c.jwt()
	if err != nil {
		return "", err
	}
	var out struct {
		Token   string    `json:"token"`
		Expires time.Time `json:"expires_at"`
	}
	err = c.request(ctx, "POST", fmt.Sprintf("%s/app/installations/%d/access_tokens", c.api, id), jwt, "installation:"+strconv.FormatInt(id, 10), map[string]any{}, &out)
	if err != nil {
		return "", err
	}
	if out.Token == "" || !out.Expires.After(time.Now()) {
		return "", domain.ErrIntegrity
	}
	s.token = out.Token
	s.expires = out.Expires
	return s.token, nil
}

type account struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"`
}
type installation struct {
	ID        int64      `json:"id"`
	AppID     int64      `json:"app_id"`
	Account   account    `json:"account"`
	Suspended *time.Time `json:"suspended_at"`
}

func (i installation) domain() domain.GitHubInstallation {
	return domain.GitHubInstallation{ID: i.ID, AccountID: i.Account.ID, Login: i.Account.Login, Kind: i.Account.Type, Suspended: i.Suspended != nil}
}
func (c *Client) Installation(ctx context.Context, id int64) (domain.GitHubInstallation, error) {
	var out installation
	jwt, err := c.jwt()
	if err != nil {
		return out.domain(), err
	}
	err = c.request(ctx, "GET", fmt.Sprintf("%s/app/installations/%d", c.api, id), jwt, "app", nil, &out)
	if err == nil && (out.ID != id || out.Account.ID <= 0 || strconv.FormatInt(out.AppID, 10) != c.cfg.AppID) {
		err = domain.ErrIntegrity
	}
	return out.domain(), err
}
func (c *Client) Authorize(ctx context.Context, code, verifier string, id int64) (out outbound.GitHubAuthorization, err error) {
	var cred struct {
		Token string `json:"access_token"`
		Error string `json:"error"`
	}
	err = c.request(ctx, "POST", c.web+"/login/oauth/access_token", "", "oauth", map[string]string{"client_id": c.cfg.ClientID, "client_secret": c.cfg.ClientSecret, "code": code, "redirect_uri": c.cfg.CallbackURL, "code_verifier": verifier}, &cred)
	if err != nil {
		return
	}
	if cred.Token == "" || cred.Error != "" {
		err = domain.ErrForbidden
		return
	}
	var user account
	err = c.request(ctx, "GET", c.api+"/user", cred.Token, "oauth", nil, &user)
	if err != nil {
		return
	}
	if user.ID <= 0 {
		err = domain.ErrIntegrity
		return
	}
	out.Identity = domain.GitHubIdentity{ExternalID: user.ID, Login: user.Login}
	if id == 0 {
		return
	}
	found := false
	for page := 1; page <= 100; page++ {
		var list struct {
			Installations []installation `json:"installations"`
		}
		err = c.request(ctx, "GET", fmt.Sprintf("%s/user/installations?per_page=100&page=%d", c.api, page), cred.Token, "oauth", nil, &list)
		if err != nil {
			return
		}
		if list.Installations == nil {
			err = domain.ErrIntegrity
			return
		}
		for _, i := range list.Installations {
			if i.ID == id {
				found = true
			}
		}
		if found || len(list.Installations) < 100 {
			break
		}
	}
	if !found {
		err = domain.ErrForbidden
		return
	}
	out.Installation, err = c.Installation(ctx, id)
	if err != nil {
		return
	}
	i := out.Installation
	if i.Suspended {
		err = domain.ErrForbidden
		return
	}
	switch i.Kind {
	case "User":
		if i.AccountID != user.ID {
			err = domain.ErrForbidden
		}
	case "Organization":
		var m struct {
			Role         string  `json:"role"`
			State        string  `json:"state"`
			Organization account `json:"organization"`
		}
		err = c.request(ctx, "GET", c.api+"/user/memberships/orgs/"+url.PathEscape(i.Login), cred.Token, "oauth", nil, &m)
		if err == nil && (m.Role != "admin" || m.State != "active" || m.Organization.ID != i.AccountID) {
			err = domain.ErrForbidden
		}
	default:
		err = domain.ErrForbidden
	}
	return
}
func (c *Client) getInstalled(ctx context.Context, id int64, path string, out any) error {
	token, err := c.InstallationToken(ctx, id)
	if err != nil {
		return err
	}
	err = c.request(ctx, "GET", c.api+path, token, "installation:"+strconv.FormatInt(id, 10), nil, out)
	if errors.Is(err, domain.ErrForbidden) {
		c.Invalidate(id)
	}
	return err
}
func (c *Client) Repositories(ctx context.Context, id int64) ([]domain.GitHubRepository, error) {
	out := []domain.GitHubRepository{}
	for page := 1; page <= 100; page++ {
		var p struct {
			Repos []struct {
				ID       int64   `json:"id"`
				FullName string  `json:"full_name"`
				Private  bool    `json:"private"`
				Owner    account `json:"owner"`
			} `json:"repositories"`
		}
		if err := c.getInstalled(ctx, id, fmt.Sprintf("/installation/repositories?per_page=100&page=%d", page), &p); err != nil {
			return nil, err
		}
		if p.Repos == nil {
			return nil, domain.ErrIntegrity
		}
		for _, r := range p.Repos {
			if r.ID <= 0 || r.Owner.ID <= 0 || !validRepositoryName(r.FullName) {
				return nil, domain.ErrIntegrity
			}
			out = append(out, domain.GitHubRepository{ID: r.ID, AccountID: r.Owner.ID, FullName: r.FullName, Private: r.Private})
		}
		if len(p.Repos) < 100 {
			return out, nil
		}
	}
	return nil, errors.New("GitHub repository list exceeds supported reconciliation size")
}
func (c *Client) Teams(ctx context.Context, id int64, org string) ([]domain.GitHubTeam, error) {
	out := []domain.GitHubTeam{}
	for page := 1; page <= 100; page++ {
		var p []domain.GitHubTeam
		if err := c.getInstalled(ctx, id, fmt.Sprintf("/orgs/%s/teams?per_page=100&page=%d", url.PathEscape(org), page), &p); err != nil {
			return nil, err
		}
		if p == nil {
			return nil, domain.ErrIntegrity
		}
		out = append(out, p...)
		if len(p) < 100 {
			return out, nil
		}
	}
	return nil, domain.ErrValidation
}
func (c *Client) TeamMembers(ctx context.Context, id int64, org, slug string) ([]int64, error) {
	out := []int64{}
	for page := 1; page <= 100; page++ {
		var p []account
		if err := c.getInstalled(ctx, id, fmt.Sprintf("/orgs/%s/teams/%s/members?per_page=100&page=%d", url.PathEscape(org), url.PathEscape(slug), page), &p); err != nil {
			return nil, err
		}
		if p == nil {
			return nil, domain.ErrIntegrity
		}
		for _, u := range p {
			if u.ID <= 0 {
				return nil, domain.ErrIntegrity
			}
			out = append(out, u.ID)
		}
		if len(p) < 100 {
			return out, nil
		}
	}
	return nil, domain.ErrValidation
}
func (c *Client) MergedPullRequests(ctx context.Context, id int64, repo domain.GitHubRepository, page int) ([]domain.PullRequestMerge, bool, error) {
	var p []struct {
		Number int        `json:"number"`
		Merged *time.Time `json:"merged_at"`
		Merge  string     `json:"merge_commit_sha"`
		Head   struct {
			Ref  string `json:"ref"`
			SHA  string `json:"sha"`
			Repo struct {
				ID int64 `json:"id"`
			} `json:"repo"`
		} `json:"head"`
		Base struct {
			Ref  string `json:"ref"`
			Repo struct {
				ID int64 `json:"id"`
			} `json:"repo"`
		} `json:"base"`
	}
	if page < 1 {
		return nil, false, domain.ErrValidation
	}
	if err := c.getInstalled(ctx, id, fmt.Sprintf("/repositories/%d/pulls?state=closed&sort=updated&direction=desc&per_page=100&page=%d", repo.ID, page), &p); err != nil {
		return nil, false, err
	}
	if p == nil {
		return nil, false, domain.ErrIntegrity
	}
	out := []domain.PullRequestMerge{}
	for _, v := range p {
		if v.Merged == nil {
			continue
		}
		if v.Base.Repo.ID != repo.ID {
			return nil, false, domain.ErrIntegrity
		}
		if v.Head.Repo.ID != repo.ID {
			continue
		}
		pr := domain.PullRequestMerge{Number: v.Number, BaseBranch: v.Base.Ref, HeadBranch: v.Head.Ref, HeadSHA: v.Head.SHA, MergeSHA: v.Merge}
		if pr.Validate() != nil {
			return nil, false, domain.ErrIntegrity
		}
		out = append(out, pr)
	}
	return out, len(p) == 100, nil
}

var repositorySegment = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func validRepositoryName(name string) bool {
	parts := strings.Split(name, "/")
	if len(parts) != 2 {
		return false
	}
	for _, p := range parts {
		if p == "." || p == ".." || !repositorySegment.MatchString(p) {
			return false
		}
	}
	return true
}
