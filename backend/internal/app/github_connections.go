package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// GitHubConnections owns binding policy. External permissions never become local roles.
type GitHubConnections struct {
	id     *IdentityService
	core   *Service
	store  outbound.GitHubStore
	remote outbound.GitHubApp
}

func NewGitHubConnections(id *IdentityService, core *Service, store outbound.GitHubStore, remote outbound.GitHubApp) *GitHubConnections {
	return &GitHubConnections{id: id, core: core, store: store, remote: remote}
}
func (g *GitHubConnections) owner(ctx context.Context, actor, nsID string) (domain.Namespace, error) {
	ns, err := g.id.organization.GetNamespace(ctx, nsID)
	if err != nil {
		return ns, err
	}
	switch ns.Kind {
	case domain.NamespaceUser:
		if ns.UserID == actor {
			return ns, nil
		}
	case domain.NamespaceOrganization:
		r, ok := g.id.OrganizationRoleOf(ctx, ns.OrganizationID, actor)
		if ok && r.AtLeast(domain.OrganizationAdmin) {
			return ns, nil
		}
	}
	return ns, domain.ErrForbidden
}
func opaqueGitHub() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func githubHash(v string) string { s := sha256.Sum256([]byte(v)); return hex.EncodeToString(s[:]) }

type GitHubStart struct {
	URL string `json:"url"`
}

func (g *GitHubConnections) Start(ctx context.Context, actor, ns string, installationID int64) (out GitHubStart, err error) {
	state, verifier := opaqueGitHub(), opaqueGitHub()
	err = g.id.withIdentity(ctx, func(tx context.Context) error {
		var gen int64
		if ns != "" {
			if _, e := g.owner(tx, actor, ns); e != nil {
				return e
			}
			c, e := g.store.GetGitHubConnection(tx, ns)
			if e == nil {
				gen = c.Generation
			} else if !errors.Is(e, domain.ErrNotFound) {
				return e
			}
		}
		if installationID < 0 || ns == "" && installationID != 0 {
			return domain.ErrValidation
		}
		return g.store.PutGitHubRequest(tx, domain.GitHubConnectRequest{Hash: githubHash(state), ActorID: actor, NamespaceID: ns, InstallationID: installationID, Verifier: verifier, Generation: gen, ExpiresAt: time.Now().Add(10 * time.Minute)})
	})
	if err != nil {
		return
	}
	out.URL = g.remote.AuthorizeURL(state, verifier)
	if ns != "" && installationID == 0 {
		out.URL = g.remote.InstallURL(state)
	}
	return
}
func (g *GitHubConnections) request(ctx context.Context, actor, state string) (domain.GitHubConnectRequest, error) {
	if len(state) != 43 {
		return domain.GitHubConnectRequest{}, domain.ErrValidation
	}
	p, err := g.store.GetGitHubRequest(ctx, githubHash(state))
	if err != nil {
		return p, err
	}
	if p.ActorID != actor || p.Consumed || !p.ExpiresAt.After(time.Now()) {
		return p, domain.ErrForbidden
	}
	if p.NamespaceID != "" {
		_, err = g.owner(ctx, actor, p.NamespaceID)
	}
	return p, err
}
func (g *GitHubConnections) Setup(ctx context.Context, actor, state string, id int64) (out GitHubStart, err error) {
	err = g.id.withIdentity(ctx, func(tx context.Context) error {
		p, e := g.request(tx, actor, state)
		if e != nil {
			return e
		}
		if p.NamespaceID == "" || id <= 0 || p.InstallationID != 0 {
			return domain.ErrValidation
		}
		p.InstallationID = id
		if e = g.store.PutGitHubRequest(tx, p); e != nil {
			return e
		}
		out.URL = g.remote.AuthorizeURL(state, p.Verifier)
		return nil
	})
	return
}
func (g *GitHubConnections) Complete(ctx context.Context, actor, state, code string) (string, error) {
	var p domain.GitHubConnectRequest
	err := g.id.withIdentity(ctx, func(tx context.Context) error {
		var e error
		p, e = g.request(tx, actor, state)
		if e != nil {
			return e
		}
		if p.NamespaceID != "" && p.InstallationID <= 0 {
			return domain.ErrValidation
		}
		consumed := p
		consumed.Consumed = true
		consumed.Verifier = ""
		return g.store.PutGitHubRequest(tx, consumed)
	})
	if err != nil {
		return "", err
	}
	proof, err := g.remote.Authorize(ctx, code, p.Verifier, p.InstallationID)
	if err != nil {
		return "", err
	}
	proof.Identity.UserID = actor
	if proof.Identity.ExternalID <= 0 {
		return "", domain.ErrIntegrity
	}
	err = g.id.withIdentity(ctx, func(tx context.Context) error {
		if p.NamespaceID == "" {
			return g.store.PutGitHubIdentity(tx, proof.Identity)
		}
		ns, e := g.owner(tx, actor, p.NamespaceID)
		if e != nil {
			return e
		}
		inst := proof.Installation
		if inst.ID != p.InstallationID || inst.Suspended || inst.AccountID <= 0 {
			return domain.ErrForbidden
		}
		if ns.Kind == domain.NamespaceUser && (inst.Kind != "User" || inst.AccountID != proof.Identity.ExternalID) || ns.Kind == domain.NamespaceOrganization && inst.Kind != "Organization" {
			return domain.ErrForbidden
		}
		c, e := g.store.GetGitHubConnection(tx, ns.ID)
		if e != nil && !errors.Is(e, domain.ErrNotFound) {
			return e
		}
		if c.Generation != p.Generation {
			return domain.ErrConflict
		}
		if c.Installation.ID != 0 && c.Installation.AccountID != inst.AccountID {
			return domain.ErrConflict
		}
		c.NamespaceID = ns.ID
		c.Installation = inst
		c.Generation++
		c.Enabled = true
		c.Status = "pending"
		c.UpdatedBy = actor
		c.NextSync = time.Now().UTC()
		if c.Bindings == nil {
			c.Bindings = []domain.GitHubBinding{}
		}
		if c.Repositories == nil {
			c.Repositories = []domain.GitHubRepository{}
		}
		if c.Teams == nil {
			c.Teams = []domain.GitHubTeam{}
		}
		if c.Mappings == nil {
			c.Mappings = []domain.GitHubTeamMapping{}
		}
		if e = g.store.PutGitHubIdentity(tx, proof.Identity); e != nil {
			return e
		}
		return g.store.PutGitHubConnection(tx, c)
	})
	return p.NamespaceID, err
}

type GitHubOwnerView struct {
	Namespace    domain.Namespace         `json:"namespace"`
	Connection   *domain.GitHubConnection `json:"connection"`
	Repositories []domain.Repository      `json:"repositories"`
	ContextRepos []domain.Repo            `json:"context_repos"`
	Teams        []domain.Team            `json:"teams"`
}
type GitHubOverview struct {
	Enabled  bool                   `json:"enabled"`
	Identity *domain.GitHubIdentity `json:"identity"`
	Owners   []GitHubOwnerView      `json:"owners"`
}

func (g *GitHubConnections) Overview(ctx context.Context, actor string) (out GitHubOverview, err error) {
	out = GitHubOverview{Enabled: true, Owners: []GitHubOwnerView{}}
	u, err := g.id.repositories.GetUser(ctx, actor)
	if err != nil {
		return out, err
	}
	personal, err := g.id.ensurePersonalNamespace(ctx, u)
	if err != nil {
		return out, err
	}
	namespaces := []domain.Namespace{personal}
	orgs, err := g.id.ListOrganizations(ctx, actor)
	if err != nil {
		return out, err
	}
	for _, o := range orgs {
		ns, e := g.owner(ctx, actor, o.NamespaceID)
		if e == nil {
			namespaces = append(namespaces, ns)
		} else if !errors.Is(e, domain.ErrForbidden) {
			return out, e
		}
	}
	ids, err := g.store.ListGitHubIdentities(ctx)
	if err != nil {
		return out, err
	}
	for _, i := range ids {
		if i.UserID == actor {
			v := i
			out.Identity = &v
		}
	}
	all, err := g.core.meta.ListRepos(ctx, "default")
	if err != nil {
		return out, err
	}
	for _, ns := range namespaces {
		v := GitHubOwnerView{Namespace: ns, Repositories: []domain.Repository{}, ContextRepos: []domain.Repo{}, Teams: []domain.Team{}}
		c, e := g.store.GetGitHubConnection(ctx, ns.ID)
		if e == nil {
			v.Connection = &c
		} else if !errors.Is(e, domain.ErrNotFound) {
			return out, e
		}
		locals, e := g.id.organization.ListRepositoriesForNamespace(ctx, ns.ID)
		if e != nil {
			return out, e
		}
		manageable := make(map[string]bool)
		for _, local := range locals {
			if g.id.IsOwner(ctx, local.ID, actor) {
				v.Repositories = append(v.Repositories, local)
				manageable[local.ID] = true
			}
		}
		// Organization administration authorizes installation management, not
		// reading or binding every private context repository in that organization.
		if v.Connection != nil {
			visible := []domain.GitHubBinding{}
			for _, b := range c.Bindings {
				if manageable[b.RepositoryID] {
					visible = append(visible, b)
				}
			}
			c.Bindings = visible
			c.PRPages = nil // Internal reconciliation cursors are not a UI contract.
		}
		for _, r := range all {
			for _, local := range v.Repositories {
				if r.RepositoryID == local.ID {
					v.ContextRepos = append(v.ContextRepos, r)
				}
			}
		}
		if ns.OrganizationID != "" {
			v.Teams, e = g.id.teams.ListTeams(ctx, ns.OrganizationID)
			if e != nil {
				return out, e
			}
		}
		out.Owners = append(out.Owners, v)
	}
	return
}
func (g *GitHubConnections) Edit(ctx context.Context, actor, ns string, generation int64, action string, binding domain.GitHubBinding, mapping domain.GitHubTeamMapping) error {
	return g.id.withIdentity(ctx, func(tx context.Context) error {
		owner, err := g.owner(tx, actor, ns)
		if err != nil {
			return err
		}
		c, err := g.store.GetGitHubConnection(tx, ns)
		if err != nil {
			return err
		}
		if c.Generation != generation {
			return domain.ErrConflict
		}
		switch action {
		case "disconnect":
			c.Enabled = false
			c.Status = "disconnected"
			c.Repositories = []domain.GitHubRepository{}
			c.Teams = []domain.GitHubTeam{}
			if err = g.store.ReplaceGitHubTeamMembers(tx, ns, nil); err != nil {
				return err
			}
			g.remote.Invalidate(c.Installation.ID)
		case "refresh":
			if !c.Enabled {
				return domain.ErrConflict
			}
		case "bind":
			if !c.Enabled || (c.Status != "connected" && c.Status != "team_access_required") {
				return domain.ErrConflict
			}
			local, e := g.id.repositories.GetRepository(tx, binding.RepositoryID)
			if e != nil {
				return e
			}
			if local.OwnerNamespaceID != ns || local.Archived || !g.id.IsOwner(tx, local.ID, actor) {
				return domain.ErrForbidden
			}
			repo, e := g.core.meta.GetRepo(tx, binding.ContextRepoID)
			if e != nil {
				return e
			}
			if repo.RepositoryID != local.ID {
				return domain.ErrForbidden
			}
			found := false
			for _, r := range c.Repositories {
				if r.ID == binding.ExternalID && r.AccountID == c.Installation.AccountID {
					found = true
					if normalizeGitURL(repo.GitRemoteURL) != normalizeGitURL("https://github.com/"+r.FullName) {
						return domain.ErrConflict
					}
				}
			}
			if !found {
				return domain.ErrForbidden
			}
			for _, b := range c.Bindings {
				if b.ContextRepoID == binding.ContextRepoID || b.ExternalID == binding.ExternalID {
					return domain.ErrConflict
				}
			}
			binding.GitOrigin = repo.GitRemoteURL
			c.Bindings = append(c.Bindings, binding)
		case "unbind":
			next := []domain.GitHubBinding{}
			for _, b := range c.Bindings {
				if b.ContextRepoID == binding.ContextRepoID {
					if !g.id.IsOwner(tx, b.RepositoryID, actor) {
						return domain.ErrForbidden
					}
					continue
				}
				next = append(next, b)
			}
			c.Bindings = next
		case "map-team":
			if !c.Enabled || c.Status != "connected" || owner.OrganizationID == "" {
				return domain.ErrForbidden
			}
			team, e := g.id.teams.GetTeam(tx, mapping.TeamID)
			if e != nil {
				return e
			}
			if team.OrganizationID != owner.OrganizationID {
				return domain.ErrForbidden
			}
			found := false
			for _, t := range c.Teams {
				if t.ID == mapping.ExternalID {
					found = true
				}
			}
			if !found {
				return domain.ErrForbidden
			}
			next := []domain.GitHubTeamMapping{}
			for _, m := range c.Mappings {
				if m.TeamID != mapping.TeamID {
					next = append(next, m)
				}
			}
			c.Mappings = append(next, mapping)
			if err = g.store.ReplaceGitHubTeamMembers(tx, ns, nil); err != nil {
				return err
			}
		case "unmap-team":
			next := []domain.GitHubTeamMapping{}
			for _, m := range c.Mappings {
				if m.TeamID != mapping.TeamID {
					next = append(next, m)
				}
			}
			c.Mappings = next
			if err = g.store.ReplaceGitHubTeamMembers(tx, ns, nil); err != nil {
				return err
			}
		default:
			return domain.ErrValidation
		}
		c.Generation++
		c.UpdatedBy = actor
		c.NextSync = time.Now().UTC()
		if owner.OrganizationID != "" {
			if err = g.id.organization.AppendOrganizationAudit(tx, organizationAudit(tx, owner.OrganizationID, actor, "github."+action, "namespace", ns, "", time.Now().UTC())); err != nil {
				return err
			}
		}
		return g.store.PutGitHubConnection(tx, c)
	})
}

type GitHubEnterpriseConnection struct {
	OrganizationID string    `json:"organization_id"`
	NamespaceID    string    `json:"namespace_id"`
	Slug           string    `json:"slug"`
	Status         string    `json:"status"`
	CheckedAt      time.Time `json:"checked_at"`
	CanManage      bool      `json:"can_manage"`
}

func (g *GitHubConnections) Enterprise(ctx context.Context, actor, id string) ([]GitHubEnterpriseConnection, error) {
	orgs, err := g.id.ListEnterpriseOrganizations(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	out := []GitHubEnterpriseConnection{}
	for _, o := range orgs {
		v := GitHubEnterpriseConnection{OrganizationID: o.ID, NamespaceID: o.NamespaceID, Slug: o.Slug, Status: "not_connected"}
		_, e := g.owner(ctx, actor, o.NamespaceID)
		v.CanManage = e == nil
		c, e := g.store.GetGitHubConnection(ctx, o.NamespaceID)
		if e == nil {
			v.Status = c.Status
			v.CheckedAt = c.CheckedAt
		} else if !errors.Is(e, domain.ErrNotFound) {
			return nil, e
		}
		out = append(out, v)
	}
	return out, nil
}

// EvidenceInstallation resolves immutable repository identity, then rewrites
// only the checked repository prefix after a GitHub rename. Stored Git history
// and its origin are not rewritten.
func (g *GitHubConnections) EvidenceInstallation(ctx context.Context, path string) (int64, string, error) {
	repoID := outbound.GitRepository(ctx)
	if repoID == "" {
		return 0, "", domain.ErrForbidden
	}
	repo, err := g.core.meta.GetRepo(ctx, repoID)
	if err != nil {
		return 0, "", err
	}
	local, err := g.id.repositories.GetRepository(ctx, repo.RepositoryID)
	if err != nil {
		return 0, "", err
	}
	c, err := g.store.GetGitHubConnection(ctx, local.OwnerNamespaceID)
	if errors.Is(err, domain.ErrNotFound) {
		return 0, path, nil
	}
	if err != nil {
		return 0, "", err
	}
	for _, b := range c.Bindings {
		if b.ContextRepoID != repoID {
			continue
		}
		current, err := g.binding(ctx, c, b)
		if err != nil {
			return 0, "", err
		}
		name := strings.TrimPrefix(normalizeGitURL(current.GitRemoteURL), "github.com/")
		prefix := "/repos/" + name + "/"
		if !strings.HasPrefix(strings.ToLower(path), prefix) {
			return 0, "", domain.ErrForbidden
		}
		for _, r := range c.Repositories {
			if r.ID == b.ExternalID {
				return c.Installation.ID, "/repos/" + r.FullName + "/" + path[len(prefix):], nil
			}
		}
	}
	return 0, "", domain.ErrForbidden
}
