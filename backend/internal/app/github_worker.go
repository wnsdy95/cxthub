package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// Receive persists verified webhook bytes before acknowledgement. Delivery IDs
// are idempotency keys; replaying a key with different bytes is a conflict.
func (g *GitHubConnections) Receive(ctx context.Context, id, kind string, body []byte) error {
	if len(id) < 1 || len(id) > 200 || len(body) > 1<<20 {
		return domain.ErrValidation
	}
	if kind == "ping" {
		return nil
	}
	switch kind {
	case "installation", "installation_repositories", "pull_request", "push", "membership", "team", "member":
	default:
		return nil
	}
	var envelope struct {
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Installation.ID <= 0 {
		return domain.ErrValidation
	}
	return g.id.withIdentity(ctx, func(tx context.Context) error {
		key := githubHash(id)
		old, e := g.store.GetGitHubDelivery(tx, key)
		if e == nil {
			if old.Kind != kind || old.BodyHash != githubHash(string(body)) {
				return domain.ErrConflict
			}
			return nil
		}
		if !errors.Is(e, domain.ErrNotFound) {
			return e
		}
		job := domain.GitHubDelivery{ID: key, Kind: kind, Body: body, BodyHash: githubHash(string(body)), NextAttempt: time.Now().UTC()}
		if e = g.store.PutGitHubDelivery(tx, job); e != nil {
			return e
		}
		// Lifecycle/member changes invalidate synchronized grants immediately. An
		// out-of-order event can only narrow access until authoritative reconciliation.
		if kind != "pull_request" && kind != "push" {
			all, e := g.store.ListGitHubConnections(tx)
			if e != nil {
				return e
			}
			for _, c := range all {
				if c.Installation.ID != envelope.Installation.ID {
					continue
				}
				c.Status = "pending"
				if !c.Enabled {
					c.Status = "disconnected"
				}
				c.Generation++
				c.NextSync = time.Now().UTC()
				if e = g.store.ReplaceGitHubTeamMembers(tx, c.NamespaceID, nil); e != nil {
					return e
				}
				if e = g.store.PutGitHubConnection(tx, c); e != nil {
					return e
				}
				g.remote.Invalidate(c.Installation.ID)
			}
		}
		return nil
	})
}
func (g *GitHubConnections) Run(ctx context.Context) {
	tick := time.NewTicker(20 * time.Second)
	defer tick.Stop()
	for {
		work, cancel := context.WithTimeout(ctx, 90*time.Second)
		if err := g.Tick(work); err != nil && ctx.Err() == nil {
			log.Printf("GitHub synchronization: %v", err)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
func (g *GitHubConnections) Tick(ctx context.Context) error {
	all, err := g.store.ListGitHubConnections(ctx)
	if err != nil {
		return err
	}
	for _, c := range all {
		if !c.Enabled || c.NextSync.After(time.Now()) {
			continue
		}
		if err = g.reconcile(ctx, c.NamespaceID); err != nil && !errors.Is(err, domain.ErrConflict) {
			return err
		}
	}
	jobs, err := g.store.ListGitHubDeliveries(ctx)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if j.Done || j.NextAttempt.After(time.Now()) || j.LeaseUntil.After(time.Now()) {
			continue
		}
		if err = g.deliver(ctx, j.ID); err != nil {
			return err
		}
	}
	return nil
}
func (g *GitHubConnections) reconcile(ctx context.Context, ns string) error {
	var c domain.GitHubConnection
	err := g.id.withIdentity(ctx, func(tx context.Context) error {
		var e error
		c, e = g.store.GetGitHubConnection(tx, ns)
		if e != nil {
			return e
		}
		if !c.Enabled || c.NextSync.After(time.Now()) {
			return domain.ErrConflict
		}
		c.NextSync = time.Now().UTC().Add(2 * time.Minute)
		return g.store.PutGitHubConnection(tx, c)
	})
	if err != nil {
		return err
	}
	lease := c.NextSync
	inst, workErr := g.remote.Installation(ctx, c.Installation.ID)
	if workErr == nil && (inst.AccountID != c.Installation.AccountID || inst.ID != c.Installation.ID || inst.Kind != c.Installation.Kind) {
		workErr = domain.ErrIntegrity
	}
	if workErr == nil && inst.Suspended {
		workErr = domain.ErrForbidden
	}
	repos := []domain.GitHubRepository{}
	teams := []domain.GitHubTeam{}
	grants := []domain.GitHubTeamMember{}
	unresolved := 0
	if workErr == nil {
		repos, workErr = g.remote.Repositories(ctx, inst.ID)
	}
	if workErr == nil {
		for _, r := range repos {
			if r.AccountID != inst.AccountID {
				workErr = domain.ErrIntegrity
				break
			}
		}
	}
	// Members:read is optional until team synchronization is configured. Repository
	// operations still work when that extra organization permission is not granted.
	teamErr := error(nil)
	if workErr == nil && inst.Kind == "Organization" {
		teams, teamErr = g.remote.Teams(ctx, inst.ID, inst.Login)
		if teamErr != nil {
			teams = []domain.GitHubTeam{}
		}
	}
	identities, identityErr := g.store.ListGitHubIdentities(ctx)
	if workErr == nil && identityErr != nil {
		workErr = identityErr
	}
	if workErr == nil && teamErr == nil {
		byID := map[int64]string{}
		for _, i := range identities {
			byID[i.ExternalID] = i.UserID
		}
		owner, e := g.id.organization.GetNamespace(ctx, ns)
		if e != nil {
			workErr = e
		} else {
			seen := map[string]bool{}
			for _, m := range c.Mappings {
				if !m.SyncMembers {
					continue
				}
				slug := ""
				for _, t := range teams {
					if t.ID == m.ExternalID {
						slug = t.Slug
					}
				}
				if slug == "" {
					continue
				}
				members, e := g.remote.TeamMembers(ctx, inst.ID, inst.Login, slug)
				if e != nil {
					workErr = e
					break
				}
				for _, id := range members {
					uid := byID[id]
					if uid == "" {
						unresolved++
						continue
					}
					if _, ok := g.id.OrganizationRoleOf(ctx, owner.OrganizationID, uid); !ok {
						unresolved++
						continue
					}
					key := m.TeamID + ":" + uid
					if !seen[key] {
						seen[key] = true
						grants = append(grants, domain.GitHubTeamMember{NamespaceID: ns, TeamID: m.TeamID, UserID: uid, ExpiresAt: time.Now().UTC().Add(10 * time.Minute)})
					}
				}
			}
		}
	}
	err = g.id.withIdentity(ctx, func(tx context.Context) error {
		current, e := g.store.GetGitHubConnection(tx, ns)
		if e != nil {
			return e
		}
		if !current.Enabled || current.Generation != c.Generation || !current.NextSync.Equal(lease) {
			return domain.ErrConflict
		}
		current.NextSync = time.Now().UTC().Add(5 * time.Minute)
		current.Status = "connected"
		current.UnresolvedMembers = unresolved
		if workErr != nil {
			current.Status = "retrying"
			current.NextSync = time.Now().UTC().Add(time.Minute)
			grants = nil
			if errors.Is(workErr, domain.ErrNotFound) || errors.Is(workErr, domain.ErrForbidden) {
				current.Status = "access_removed"
				current.Repositories = []domain.GitHubRepository{}
				current.Teams = []domain.GitHubTeam{}
			}
		} else {
			current.Installation = inst
			current.Repositories = repos
			current.Teams = teams
			current.CheckedAt = time.Now().UTC()
		}
		if teamErr != nil && len(current.Mappings) > 0 {
			current.Status = "team_access_required"
			grants = nil
		}
		// Membership may have been revoked while GitHub was responding.
		owner, e := g.id.organization.GetNamespace(tx, ns)
		if e != nil {
			return e
		}
		valid := []domain.GitHubTeamMember{}
		for _, grant := range grants {
			team, e := g.id.teams.GetTeam(tx, grant.TeamID)
			if e != nil {
				if errors.Is(e, domain.ErrNotFound) {
					continue
				}
				return e
			}
			if team.OrganizationID != owner.OrganizationID {
				continue
			}
			if _, ok := g.id.OrganizationRoleOf(tx, owner.OrganizationID, grant.UserID); ok {
				valid = append(valid, grant)
			}
		}
		if e = g.store.ReplaceGitHubTeamMembers(tx, ns, valid); e != nil {
			return e
		}
		if e = g.store.PutGitHubConnection(tx, current); e != nil {
			return e
		}
		c = current
		return nil
	})
	if err != nil {
		return err
	}
	if workErr != nil {
		return nil
	}
	// Repeated full pagination repairs missed webhook deliveries. Cursor progress
	// is persisted only after every PR on that page is durably queued.
	for _, b := range c.Bindings {
		var remote domain.GitHubRepository
		for _, r := range repos {
			if r.ID == b.ExternalID {
				remote = r
			}
		}
		if remote.ID == 0 {
			continue
		}
		page := c.PRPages[remote.ID]
		if page < 1 {
			page = 1
		}
		prs, more, e := g.remote.MergedPullRequests(ctx, inst.ID, remote, page)
		if e != nil {
			continue
		}
		for _, pr := range prs {
			if e = g.enqueuePR(ctx, c, b, pr); e != nil {
				break
			}
		}
		if e != nil {
			continue
		}
		next := 1
		if more {
			next = page + 1
		}
		e = g.id.withIdentity(ctx, func(tx context.Context) error {
			v, e := g.store.GetGitHubConnection(tx, ns)
			if e != nil {
				return e
			}
			if v.Generation != c.Generation || !v.Enabled {
				return domain.ErrConflict
			}
			if v.PRPages == nil {
				v.PRPages = map[int64]int{}
			}
			v.PRPages[remote.ID] = next
			return g.store.PutGitHubConnection(tx, v)
		})
		if e != nil {
			return e
		}
	}
	return nil
}
func (g *GitHubConnections) binding(ctx context.Context, c domain.GitHubConnection, b domain.GitHubBinding) (domain.Repo, error) {
	current, err := g.store.GetGitHubConnection(ctx, c.NamespaceID)
	if err != nil {
		return domain.Repo{}, err
	}
	if !current.Enabled || current.Generation != c.Generation || current.Installation.Suspended || current.CheckedAt.Before(time.Now().Add(-10*time.Minute)) || (current.Status != "connected" && current.Status != "team_access_required") {
		return domain.Repo{}, domain.ErrForbidden
	}
	found := false
	for _, v := range current.Bindings {
		if v == b {
			found = true
		}
	}
	if !found {
		return domain.Repo{}, domain.ErrForbidden
	}
	name := ""
	for _, r := range current.Repositories {
		if r.ID == b.ExternalID {
			name = r.FullName
		}
	}
	if name == "" {
		return domain.Repo{}, domain.ErrForbidden
	}
	local, err := g.id.repositories.GetRepository(ctx, b.RepositoryID)
	if err != nil {
		return domain.Repo{}, err
	}
	if local.OwnerNamespaceID != c.NamespaceID || local.Archived {
		return domain.Repo{}, domain.ErrForbidden
	}
	repo, err := g.core.meta.GetRepo(ctx, b.ContextRepoID)
	if err != nil {
		return repo, err
	}
	if repo.RepositoryID != b.RepositoryID || normalizeGitURL(repo.GitRemoteURL) != normalizeGitURL(b.GitOrigin) {
		return repo, domain.ErrConflict
	}
	return repo, nil
}
func (g *GitHubConnections) enqueuePR(ctx context.Context, c domain.GitHubConnection, b domain.GitHubBinding, pr domain.PullRequestMerge) error {
	return repositoryWriteError(inbound.WithSystemActor(ctx), g.core, b.ContextRepoID, func(tx context.Context) error {
		if _, e := g.binding(tx, c, b); e != nil {
			return e
		}
		_, err := g.core.submitPRPromotion(tx, b.ContextRepoID, pr)
		return err
	})
}
func (g *GitHubConnections) deliver(ctx context.Context, id string) error {
	var job domain.GitHubDelivery
	err := g.id.withIdentity(ctx, func(tx context.Context) error {
		var e error
		job, e = g.store.GetGitHubDelivery(tx, id)
		if e != nil {
			return e
		}
		if job.Done || job.LeaseUntil.After(time.Now()) || job.NextAttempt.After(time.Now()) {
			return domain.ErrConflict
		}
		job.Lease = opaqueGitHub()
		job.LeaseUntil = time.Now().Add(2 * time.Minute)
		job.Attempts++
		return g.store.PutGitHubDelivery(tx, job)
	})
	if errors.Is(err, domain.ErrConflict) {
		return nil
	}
	if err != nil {
		return err
	}
	workErr := g.applyDelivery(ctx, job)
	return g.id.withIdentity(ctx, func(tx context.Context) error {
		current, e := g.store.GetGitHubDelivery(tx, id)
		if e != nil {
			return e
		}
		if current.Lease != job.Lease {
			return domain.ErrConflict
		}
		current.LeaseUntil = time.Time{}
		current.Done = workErr == nil
		if current.Done {
			current.Body = nil
		} // Retain idempotency proof, not completed webhook contents.
		current.NextAttempt = time.Now().Add(time.Duration(min(current.Attempts, 30)) * time.Minute)
		return g.store.PutGitHubDelivery(tx, current)
	})
}
func (g *GitHubConnections) applyDelivery(ctx context.Context, j domain.GitHubDelivery) error {
	if j.Kind != "pull_request" && j.Kind != "push" {
		return nil
	}
	var p struct {
		Action       string `json:"action"`
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
		Repository struct {
			ID int64 `json:"id"`
		} `json:"repository"`
		Number int `json:"number"`
		PR     struct {
			Merged bool   `json:"merged"`
			Merge  string `json:"merge_commit_sha"`
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
		} `json:"pull_request"`
		Ref    string `json:"ref"`
		Before string `json:"before"`
		After  string `json:"after"`
		Forced bool   `json:"forced"`
	}
	if json.Unmarshal(j.Body, &p) != nil {
		return domain.ErrValidation
	}
	if j.Kind == "pull_request" && (p.Action != "closed" || !p.PR.Merged || p.PR.Head.Repo.ID != p.Repository.ID) {
		return nil
	}
	all, err := g.store.ListGitHubConnections(ctx)
	if err != nil {
		return err
	}
	for _, c := range all {
		if c.Installation.ID != p.Installation.ID || !c.Enabled {
			continue
		}
		for _, b := range c.Bindings {
			if b.ExternalID != p.Repository.ID {
				continue
			}
			if j.Kind == "pull_request" {
				if p.PR.Base.Repo.ID != p.Repository.ID {
					return domain.ErrIntegrity
				}
				pr := domain.PullRequestMerge{Number: p.Number, BaseBranch: p.PR.Base.Ref, HeadBranch: p.PR.Head.Ref, HeadSHA: p.PR.Head.SHA, MergeSHA: p.PR.Merge}
				if err = g.enqueuePR(ctx, c, b, pr); err != nil {
					return err
				}
			} else {
				if !strings.HasPrefix(p.Ref, "refs/heads/") {
					continue
				}
				zero := func(s string) string {
					if (len(s) == 40 || len(s) == 64) && strings.Trim(s, "0") == "" {
						return ""
					}
					return s
				}
				err = repositoryWriteError(evidenceWriteContext(inbound.WithSystemActor(ctx)), g.core, b.ContextRepoID, func(tx context.Context) error {
					repo, e := g.binding(tx, c, b)
					if e != nil {
						return e
					}
					st, ok := g.core.meta.(outbound.GitScanStore)
					if !ok {
						return fmt.Errorf("Git scan queue unavailable")
					}
					observation := domain.GitRefObservation{RepoID: repo.ID, GitOrigin: repo.GitRemoteURL, Source: "github-push", Ref: p.Ref, Before: zero(p.Before), After: zero(p.After), Forced: p.Forced, Delivery: j.ID}.WithID()
					return st.RecordGitRefObservation(tx, observation, time.Now().UTC())
				})
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}
