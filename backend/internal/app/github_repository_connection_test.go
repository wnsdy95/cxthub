package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// failingGitHubStore reads a connection well a number of times, then fails.
type failingGitHubStore struct {
	outbound.GitHubStore
	reads int
}

var errGitHubStoreDown = errors.New("the store is down")

func (s *failingGitHubStore) GetGitHubConnection(ctx context.Context, id string) (domain.GitHubConnection, error) {
	if s.reads--; s.reads < 0 {
		return domain.GitHubConnection{}, errGitHubStoreDown
	}
	return s.GitHubStore.GetGitHubConnection(ctx, id)
}

// A repository is connected with GitHub exactly while the server would read it
// through its owner's installation: the same repository, at the same moment,
// is given the installation by EvidenceInstallation.
func TestRepositoryConnectionIsWhatTheReaderOfGitHubIsGiven(t *testing.T) {
	g, st, f, remote := githubFixture(t)
	ctx := context.Background()
	ns := f.organization.NamespaceID
	remote.repos = []domain.GitHubRepository{{ID: 91, AccountID: 456, FullName: "acme/api"}}
	repo := domain.Repo{ID: hh("github-connected"), RepositoryID: f.repository.ID, DefaultBranch: "main", GitRemoteURL: "https://github.com/acme/api.git"}
	second, err := f.identity.CreateOrganizationRepository(systemTestContext(), f.owner, f.organization.ID, "Other")
	if err != nil {
		t.Fatal(err)
	}
	other := domain.Repo{ID: hh("github-other-repository"), RepositoryID: second.ID, DefaultBranch: "main", GitRemoteURL: "https://github.com/acme/other.git"}
	unowned := domain.Repo{ID: hh("github-unowned"), DefaultBranch: "main", GitRemoteURL: "https://github.com/acme/unowned.git"}
	for _, stored := range []domain.Repo{repo, other, unowned} {
		if _, err := st.PutRepo(ctx, stored); err != nil {
			t.Fatal(err)
		}
	}
	expect := func(what string, id domain.ContentHash, connected bool, namespace string) {
		t.Helper()
		got, owner, err := g.RepositoryConnection(ctx, id)
		if err != nil || got != connected || owner != namespace {
			t.Fatalf("%s: connected %v, owner %q, %v", what, got, owner, err)
		}
		installation, _, err := g.EvidenceInstallation(outbound.WithGitRepository(ctx, id), "/repos/acme/api/git/commits/abc")
		if given := err == nil && installation != 0; given != connected {
			t.Fatalf("%s: connected %v, but the reader of GitHub is given installation %d (%v)", what, connected, installation, err)
		}
	}
	edit := func(change func(*domain.GitHubConnection)) {
		t.Helper()
		c, err := st.GetGitHubConnection(ctx, ns)
		if err != nil {
			t.Fatal(err)
		}
		change(&c)
		if err = st.PutGitHubConnection(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	reconcile := func() {
		t.Helper()
		edit(func(c *domain.GitHubConnection) { c.NextSync = time.Time{} })
		if err := githubWorkerOf(g).reconcile(ctx, ns); err != nil {
			t.Fatal(err)
		}
	}

	if _, _, err := g.RepositoryConnection(ctx, hh("github-no-such-repository")); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a repository the server does not know: %v", err)
	}
	// A repository no account owns has no owner who could connect it.
	if connected, owner, err := g.RepositoryConnection(ctx, unowned.ID); err != nil || connected || owner != "" {
		t.Fatalf("a repository without an owner: %v %q %v", connected, owner, err)
	}
	expect("the owner has no installation", repo.ID, false, ns)

	if _, err := g.Complete(ctx, f.owner.ID, startGitHub(t, g, f.owner.ID, ns), "code"); err != nil {
		t.Fatal(err)
	}
	reconcile()
	expect("an installation that was not told to read the repository", repo.ID, false, ns)

	c, _ := st.GetGitHubConnection(ctx, ns)
	if err := g.Edit(ctx, f.owner.ID, ns, c.Generation, "bind", domain.GitHubBinding{RepositoryID: f.repository.ID, ContextRepoID: repo.ID, ExternalID: 91}, domain.GitHubTeamMapping{}); err != nil {
		t.Fatal(err)
	}
	expect("a bound repository", repo.ID, true, ns)
	expect("another repository of the same owner", other.ID, false, ns)

	// The installation is used only while GitHub was heard from lately.
	edit(func(c *domain.GitHubConnection) { c.CheckedAt = time.Now().Add(-11 * time.Minute) })
	expect("an installation last checked eleven minutes ago", repo.ID, false, ns)
	reconcile()
	expect("the installation after it was checked again", repo.ID, true, ns)

	edit(func(c *domain.GitHubConnection) { c.Enabled = false })
	expect("an installation that was switched off, whatever its status says", repo.ID, false, ns)
	edit(func(c *domain.GitHubConnection) { c.Enabled = true; c.Installation.Suspended = true })
	expect("a suspended installation", repo.ID, false, ns)
	edit(func(c *domain.GitHubConnection) { c.Installation.Suspended = false; c.Status = "retrying" })
	expect("an installation GitHub did not answer for", repo.ID, false, ns)
	edit(func(c *domain.GitHubConnection) { c.Status = "team_access_required" })
	expect("an installation that lacks only the team permission", repo.ID, true, ns)

	// The repository on GitHub was taken out of the installation.
	remote.repos = nil
	reconcile()
	expect("a repository the installation no longer reaches", repo.ID, false, ns)
	remote.repos = []domain.GitHubRepository{{ID: 91, AccountID: 456, FullName: "acme/api"}}
	reconcile()
	expect("the repository after it was given back", repo.ID, true, ns)

	// The clone the binding was made for now names another origin.
	origin := ""
	edit(func(c *domain.GitHubConnection) {
		origin, c.Bindings[0].GitOrigin = c.Bindings[0].GitOrigin, "github.com/acme/moved"
	})
	expect("a binding made for another origin", repo.ID, false, ns)
	edit(func(c *domain.GitHubConnection) { c.Bindings[0].GitOrigin = origin })
	expect("the binding of this origin", repo.ID, true, ns)

	// A store that fails is an error, never "not connected".
	for reads := range 2 {
		config := g.GitHubWorkerConfig(GitHubWorkerCompatibility)
		config.Store = &failingGitHubStore{GitHubStore: st, reads: reads}
		worker, err := NewGitHubWorker(config)
		if err != nil {
			t.Fatal(err)
		}
		if connected, owner, err := worker.RepositoryConnection(ctx, repo.ID); !errors.Is(err, errGitHubStoreDown) || connected || owner != "" {
			t.Fatalf("a store that fails at read %d: %v %q %v", reads+1, connected, owner, err)
		}
	}

	c, _ = st.GetGitHubConnection(ctx, ns)
	if err := g.Edit(ctx, f.owner.ID, ns, c.Generation, "disconnect", domain.GitHubBinding{}, domain.GitHubTeamMapping{}); err != nil {
		t.Fatal(err)
	}
	expect("a disconnected installation", repo.ID, false, ns)
}
