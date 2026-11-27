package app

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// The code of a repository can lie in the GitHub account of somebody who does
// not own the repository here. Whoever maintains the repository and manages
// an installation that reaches its Git origin connects the two, whichever
// owner space the repository is in.
func TestGitHubMaintainerBindsARepositoryOfAnotherOwnerSpace(t *testing.T) {
	g, st, f, remote := githubFixture(t)
	ctx := context.Background()
	ns := f.organization.NamespaceID
	remote.repos = []domain.GitHubRepository{{ID: 91, AccountID: 456, FullName: "acme/api"}}
	if _, err := g.Complete(ctx, f.owner.ID, startGitHub(t, g, f.owner.ID, ns), "code"); err != nil {
		t.Fatal(err)
	}
	if err := githubWorkerOf(g).reconcile(ctx, ns); err != nil {
		t.Fatal(err)
	}
	// The repository is the outsider's own, in the outsider's personal space.
	side, err := f.identity.CreateRepository(systemTestContext(), f.outsider, "Side")
	if err != nil || side.OwnerNamespaceID == ns || side.OwnerNamespaceID == "" {
		t.Fatalf("the outsider's repository: %+v %v", side, err)
	}
	stored := domain.Repo{ID: hh("github-side-repository"), RepositoryID: side.ID, DefaultBranch: "main", GitRemoteURL: "https://github.com/acme/api.git"}
	if _, err := st.PutRepo(ctx, stored); err != nil {
		t.Fatal(err)
	}
	binding := domain.GitHubBinding{RepositoryID: side.ID, ContextRepoID: stored.ID, ExternalID: 91}
	scoped := outbound.WithGitRepository(ctx, stored.ID)
	const path = "/repos/acme/api/git/commits/abc"
	listed := func(actor string) (repositories, stores, bindings int) {
		t.Helper()
		view, err := g.Overview(ctx, actor)
		if err != nil {
			t.Fatal(err)
		}
		for _, owner := range view.Owners {
			if owner.Namespace.ID != ns {
				continue
			}
			for _, repository := range owner.Repositories {
				if repository.ID == side.ID {
					repositories++
				}
			}
			for _, repo := range owner.ContextRepos {
				if repo.ID == stored.ID {
					stores++
				}
			}
			for _, bound := range owner.Connection.Bindings {
				if bound.ContextRepoID == stored.ID {
					bindings++
				}
			}
		}
		return
	}
	bind := func(actor, action string) error {
		t.Helper()
		c, err := st.GetGitHubConnection(ctx, ns)
		if err != nil {
			t.Fatal(err)
		}
		return g.Edit(ctx, actor, ns, c.Generation, action, binding, domain.GitHubTeamMapping{})
	}
	role := func(to domain.MemberRole) {
		t.Helper()
		if err := st.AddMember(ctx, domain.Membership{RepositoryID: side.ID, UserID: f.owner.ID, Role: to}); err != nil {
			t.Fatal(err)
		}
	}

	// Somebody who manages the installation and has no part in the repository
	// neither sees it nor binds it; nor does a member of it.
	for _, as := range []domain.MemberRole{"", domain.RoleMember} {
		if as != "" {
			role(as)
		}
		if repositories, stores, _ := listed(f.owner.ID); repositories != 0 || stores != 0 {
			t.Fatalf("with the role %q the repository is offered: %d %d", as, repositories, stores)
		}
		if err := bind(f.owner.ID, "bind"); !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("with the role %q the repository was bound: %v", as, err)
		}
	}
	// While nobody was told to read it and its owner has no installation, the
	// server reads it as it reads a repository without the App.
	if connected, err := g.RepositoryConnection(ctx, stored.ID); err != nil || connected {
		t.Fatalf("before the binding: %v %v", connected, err)
	}
	if installation, rewritten, err := g.EvidenceInstallation(scoped, path); err != nil || installation != 0 || rewritten != path {
		t.Fatalf("before the binding the reader of GitHub is given %d %q %v", installation, rewritten, err)
	}

	// A maintainer of the repository is offered it under the installation they
	// manage, and binds it.
	role(domain.RoleMaintainer)
	if repositories, stores, bindings := listed(f.owner.ID); repositories != 1 || stores != 1 || bindings != 0 {
		t.Fatalf("a maintainer is offered: %d %d %d", repositories, stores, bindings)
	}
	if err := bind(f.owner.ID, "bind"); err != nil {
		t.Fatalf("a maintainer binds the repository: %v", err)
	}
	if _, _, bindings := listed(f.owner.ID); bindings != 1 {
		t.Fatalf("the maintainer sees %d bindings of it", bindings)
	}
	// The server now reads the repository through that installation, although
	// the installation belongs to another owner space than the repository.
	if connected, err := g.RepositoryConnection(ctx, stored.ID); err != nil || !connected {
		t.Fatalf("after the binding: %v %v", connected, err)
	}
	if installation, rewritten, err := g.EvidenceInstallation(scoped, path); err != nil || installation != 789 || rewritten != path {
		t.Fatalf("after the binding the reader of GitHub is given %d %q %v", installation, rewritten, err)
	}
	// The owner of the repository manages no installation: they are shown no
	// connection of that space.
	if repositories, stores, bindings := listed(f.outsider.ID); repositories != 0 || stores != 0 || bindings != 0 {
		t.Fatalf("the owner of the repository is shown the other space's connection: %d %d %d", repositories, stores, bindings)
	}

	// A repository is read through one installation: no second connection
	// binds it.
	personal, err := f.identity.ensurePersonalNamespace(systemTestContext(), f.outsider)
	if err != nil {
		t.Fatal(err)
	}
	own := domain.GitHubConnection{NamespaceID: personal.ID, Installation: domain.GitHubInstallation{ID: 790, AccountID: 457, Kind: "User", Login: "outsider"}, Enabled: true, Status: "connected",
		Repositories: []domain.GitHubRepository{{ID: 92, AccountID: 457, FullName: "acme/api"}}, Bindings: []domain.GitHubBinding{}}
	if err := st.PutGitHubConnection(ctx, own); err != nil {
		t.Fatal(err)
	}
	second := domain.GitHubBinding{RepositoryID: side.ID, ContextRepoID: stored.ID, ExternalID: 92}
	if err := g.Edit(ctx, f.outsider.ID, personal.ID, own.Generation, "bind", second, domain.GitHubTeamMapping{}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a second connection bound the repository: %v", err)
	}

	// Somebody who no longer maintains the repository does not unbind it.
	role(domain.RoleMember)
	if err := bind(f.owner.ID, "unbind"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a member unbound the repository: %v", err)
	}
	if _, _, bindings := listed(f.owner.ID); bindings != 0 {
		t.Fatalf("a member is shown %d bindings of the repository", bindings)
	}
	if connected, _ := g.RepositoryConnection(ctx, stored.ID); !connected {
		t.Fatal("the binding ended when its maker stopped maintaining the repository")
	}
	role(domain.RoleMaintainer)
	if err := bind(f.owner.ID, "unbind"); err != nil {
		t.Fatalf("a maintainer unbinds the repository: %v", err)
	}
	// Unbound again, and its owner now has an installation: that installation
	// reads only what it was told to, and nothing else reads in its place.
	if connected, err := g.RepositoryConnection(ctx, stored.ID); err != nil || connected {
		t.Fatalf("after the unbinding: %v %v", connected, err)
	}
	if installation, _, err := g.EvidenceInstallation(scoped, path); !errors.Is(err, domain.ErrForbidden) || installation != 0 {
		t.Fatalf("after the unbinding the reader of GitHub is given %d %v", installation, err)
	}
	// Its owner binds it through their own installation.
	if err := g.Edit(ctx, f.outsider.ID, personal.ID, own.Generation, "bind", second, domain.GitHubTeamMapping{}); err != nil {
		t.Fatalf("the owner binds the repository through their own installation: %v", err)
	}
	// An archived repository is bound by nobody.
	archived, err := f.identity.CreateRepository(systemTestContext(), f.outsider, "Old")
	if err != nil {
		t.Fatal(err)
	}
	archived.Archived = true
	if err := st.CreateRepository(ctx, archived); err != nil {
		t.Fatal(err)
	}
	old := domain.Repo{ID: hh("github-archived-repository"), RepositoryID: archived.ID, DefaultBranch: "main", GitRemoteURL: "https://github.com/acme/api.git"}
	if _, err := st.PutRepo(ctx, old); err != nil {
		t.Fatal(err)
	}
	c, _ := st.GetGitHubConnection(ctx, personal.ID)
	if err := g.Edit(ctx, f.outsider.ID, personal.ID, c.Generation, "bind", domain.GitHubBinding{RepositoryID: archived.ID, ContextRepoID: old.ID, ExternalID: 92}, domain.GitHubTeamMapping{}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("an archived repository was bound: %v", err)
	}
}

// githubBindingLookup is what both stores answer when asked which connection
// binds a repository: the one connection that does, found by the repository
// alone, and nothing for a repository no connection binds.
func githubBindingLookup(t *testing.T, st outbound.GitHubStore, f teamFixture, installation int64) {
	t.Helper()
	ctx := context.Background()
	personal, err := f.identity.ensurePersonalNamespace(systemTestContext(), f.outsider)
	if err != nil {
		t.Fatal(err)
	}
	// The store of a test run can hold what earlier runs left: the
	// repositories are named after this run's installation.
	run := fmt.Sprintf("%s %d", t.Name(), installation)
	first, second, third := hh(run+" first"), hh(run+" second"), hh(run+" third")
	organization := domain.GitHubConnection{NamespaceID: f.organization.NamespaceID, Installation: domain.GitHubInstallation{ID: installation, AccountID: 456, Kind: "Organization", Login: "acme"}, Enabled: true,
		Bindings: []domain.GitHubBinding{{RepositoryID: f.repository.ID, ContextRepoID: first, ExternalID: 91}}}
	own := domain.GitHubConnection{NamespaceID: personal.ID, Installation: domain.GitHubInstallation{ID: installation + 1, AccountID: 457, Kind: "User", Login: "outsider"}, Enabled: true,
		Bindings: []domain.GitHubBinding{{RepositoryID: f.repository.ID, ContextRepoID: second, ExternalID: 92}, {RepositoryID: f.repository.ID, ContextRepoID: third, ExternalID: 93}}}
	for _, connection := range []domain.GitHubConnection{organization, own} {
		if err := st.PutGitHubConnection(ctx, connection); err != nil {
			t.Fatal(err)
		}
	}
	for repo, namespace := range map[domain.ContentHash]string{first: organization.NamespaceID, second: own.NamespaceID, third: own.NamespaceID} {
		if found, err := st.FindGitHubBinding(ctx, repo); err != nil || found.NamespaceID != namespace || found.Installation.ID == 0 {
			t.Fatalf("the connection that binds %s: %q %v, expected %q", repo, found.NamespaceID, err, namespace)
		}
	}
	// A repository no connection binds, and a value that is in a connection
	// but names no bound repository.
	for _, repo := range []domain.ContentHash{hh(run + " unbound"), domain.ContentHash(f.repository.ID), ""} {
		if found, err := st.FindGitHubBinding(ctx, repo); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("a connection was found for %q: %q %v", repo, found.NamespaceID, err)
		}
	}
	organization.Bindings = []domain.GitHubBinding{}
	if err := st.PutGitHubConnection(ctx, organization); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FindGitHubBinding(ctx, first); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a repository that was unbound is still found: %v", err)
	}
}

func TestGitHubBindingIsFoundByItsRepository(t *testing.T) {
	_, st, f, _ := githubFixture(t)
	githubBindingLookup(t, st, f, 789)
}

// What is told of a repository is told of that repository: another
// repository the same installation reads well does not answer for it.
func TestRepositoryConnectionAnswersForTheRepositoryAskedAbout(t *testing.T) {
	g, st, f, remote := githubFixture(t)
	ctx := context.Background()
	ns := f.organization.NamespaceID
	// Two GitHub repositories, and a third identifier for the first one's
	// name, so that each rule about binding twice is met alone.
	remote.repos = []domain.GitHubRepository{{ID: 91, AccountID: 456, FullName: "acme/api"}, {ID: 92, AccountID: 456, FullName: "acme/other"}, {ID: 93, AccountID: 456, FullName: "acme/api"}}
	if _, err := g.Complete(ctx, f.owner.ID, startGitHub(t, g, f.owner.ID, ns), "code"); err != nil {
		t.Fatal(err)
	}
	if err := githubWorkerOf(g).reconcile(ctx, ns); err != nil {
		t.Fatal(err)
	}
	second, err := f.identity.CreateOrganizationRepository(systemTestContext(), f.owner, f.organization.ID, "Other")
	if err != nil {
		t.Fatal(err)
	}
	api := domain.Repo{ID: hh("github-asked-api"), RepositoryID: f.repository.ID, DefaultBranch: "main", GitRemoteURL: "https://github.com/acme/api.git"}
	other := domain.Repo{ID: hh("github-asked-other"), RepositoryID: second.ID, DefaultBranch: "main", GitRemoteURL: "https://github.com/acme/other.git"}
	for index, stored := range []domain.Repo{api, other} {
		if _, err := st.PutRepo(ctx, stored); err != nil {
			t.Fatal(err)
		}
		c, _ := st.GetGitHubConnection(ctx, ns)
		if err := g.Edit(ctx, f.owner.ID, ns, c.Generation, "bind", domain.GitHubBinding{RepositoryID: stored.RepositoryID, ContextRepoID: stored.ID, ExternalID: int64(91 + index)}, domain.GitHubTeamMapping{}); err != nil {
			t.Fatal(err)
		}
	}
	// The same repository cannot be bound a second time, nor a GitHub
	// repository that is bound already.
	third, err := f.identity.CreateOrganizationRepository(systemTestContext(), f.owner, f.organization.ID, "Third")
	if err != nil {
		t.Fatal(err)
	}
	mirror := domain.Repo{ID: hh("github-asked-third"), RepositoryID: third.ID, DefaultBranch: "main", GitRemoteURL: "https://github.com/acme/other.git"}
	if _, err := st.PutRepo(ctx, mirror); err != nil {
		t.Fatal(err)
	}
	c, _ := st.GetGitHubConnection(ctx, ns)
	for name, again := range map[string]domain.GitHubBinding{
		"a repository that is bound":        {RepositoryID: api.RepositoryID, ContextRepoID: api.ID, ExternalID: 93},
		"a GitHub repository that is bound": {RepositoryID: third.ID, ContextRepoID: mirror.ID, ExternalID: 92},
	} {
		if err := g.Edit(ctx, f.owner.ID, ns, c.Generation, "bind", again, domain.GitHubTeamMapping{}); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("%s was bound again: %v", name, err)
		}
	}
	if c, _ = st.GetGitHubConnection(ctx, ns); len(c.Bindings) != 2 {
		t.Fatalf("the connection holds %d bindings", len(c.Bindings))
	}
	// The binding of the first repository was made for another origin; the
	// second one is as it was.
	for index := range c.Bindings {
		if c.Bindings[index].ContextRepoID == api.ID {
			c.Bindings[index].GitOrigin = "github.com/acme/moved"
		}
	}
	if err := st.PutGitHubConnection(ctx, c); err != nil {
		t.Fatal(err)
	}
	for repo, connected := range map[domain.ContentHash]bool{api.ID: false, other.ID: true} {
		if got, err := g.RepositoryConnection(ctx, repo); err != nil || got != connected {
			t.Fatalf("%s: connected %v, expected %v: %v", repo, got, connected, err)
		}
	}
	if installation, _, err := g.EvidenceInstallation(outbound.WithGitRepository(ctx, other.ID), "/repos/acme/other/git/commits/abc"); err != nil || installation != 789 {
		t.Fatalf("the reader of GitHub is given %d %v for the repository that is bound well", installation, err)
	}
	if installation, _, err := g.EvidenceInstallation(outbound.WithGitRepository(ctx, api.ID), "/repos/acme/api/git/commits/abc"); err == nil || installation != 0 {
		t.Fatalf("the reader of GitHub is given %d for the repository whose binding is for another origin", installation)
	}
}

// An owner of an organization holds the owner's role in its repositories
// without being a member of each: they are offered those repositories too.
func TestGitHubOrganizationOwnerIsOfferedTheOrganizationsRepositories(t *testing.T) {
	g, st, f, remote := githubFixture(t)
	ctx := context.Background()
	ns := f.organization.NamespaceID
	remote.repos = []domain.GitHubRepository{{ID: 91, AccountID: 456, FullName: "acme/api"}}
	if _, err := g.Complete(ctx, f.owner.ID, startGitHub(t, g, f.owner.ID, ns), "code"); err != nil {
		t.Fatal(err)
	}
	if err := githubWorkerOf(g).reconcile(ctx, ns); err != nil {
		t.Fatal(err)
	}
	stored := domain.Repo{ID: hh("github-organization-owner"), RepositoryID: f.repository.ID, DefaultBranch: "main", GitRemoteURL: "https://github.com/acme/api.git"}
	if _, err := st.PutRepo(ctx, stored); err != nil {
		t.Fatal(err)
	}
	if err := f.identity.UpdateOrganizationMember(systemTestContext(), f.owner.ID, f.organization.ID, f.member.ID, domain.OrganizationOwner); err != nil {
		t.Fatal(err)
	}
	if member, err := st.IsMember(ctx, f.repository.ID, f.member.ID); err != nil || member {
		t.Fatalf("the second owner of the organization is a member of the repository itself: %v %v", member, err)
	}
	view, err := g.Overview(ctx, f.member.ID)
	if err != nil {
		t.Fatal(err)
	}
	offered := 0
	for _, owner := range view.Owners {
		if owner.Namespace.ID != ns {
			continue
		}
		for _, repo := range owner.ContextRepos {
			if repo.ID == stored.ID {
				offered++
			}
		}
	}
	if offered != 1 {
		t.Fatalf("an owner of the organization is offered its repository %d times", offered)
	}
	c, _ := st.GetGitHubConnection(ctx, ns)
	if err := g.Edit(ctx, f.member.ID, ns, c.Generation, "bind", domain.GitHubBinding{RepositoryID: f.repository.ID, ContextRepoID: stored.ID, ExternalID: 91}, domain.GitHubTeamMapping{}); err != nil {
		t.Fatalf("an owner of the organization binds its repository: %v", err)
	}
}
