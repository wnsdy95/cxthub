package app

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type githubFake struct {
	repos       []domain.GitHubRepository
	members     []int64
	onMembers   func()
	unavailable bool

	outbound.GitHubApp
	proof       outbound.GitHubAuthorization
	onAuthorize func()
	calls       int
}

func (f *githubFake) InstallURL(state string) string {
	return "https://github.com/install?state=" + state
}
func (f *githubFake) AuthorizeURL(state, verifier string) string {
	return "https://github.com/authorize?state=" + state
}
func (f *githubFake) Authorize(context.Context, string, string, int64) (outbound.GitHubAuthorization, error) {
	f.calls++
	if f.onAuthorize != nil {
		f.onAuthorize()
	}
	return f.proof, nil
}
func (f *githubFake) Invalidate(int64) {}
func githubFixture(t *testing.T) (*GitHubConnections, *store.FSStore, teamFixture, *githubFake) {
	t.Helper()
	st := store.NewFSStore(t.TempDir())
	f := makeTeamFixture(t, st)
	remote := &githubFake{proof: outbound.GitHubAuthorization{Identity: domain.GitHubIdentity{ExternalID: 123, Login: "operator"}, Installation: domain.GitHubInstallation{ID: 789, AccountID: 456, Kind: "Organization", Login: "acme"}}}
	g := NewGitHubConnections(f.identity, &Service{meta: st, repositories: st}, st, remote)
	return g, st, f, remote
}
func startGitHub(t *testing.T, g *GitHubConnections, actor, ns string) string {
	t.Helper()
	out, err := g.Start(context.Background(), actor, ns, 789)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(out.URL)
	return u.Query().Get("state")
}
func TestGitHubCallbackBindsActorAndConsumesState(t *testing.T) {
	g, st, f, remote := githubFixture(t)
	ctx := context.Background()
	state := startGitHub(t, g, f.owner.ID, f.organization.NamespaceID)
	if _, err := g.Complete(ctx, f.outsider.ID, state, "code"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("wrong actor: %v", err)
	}
	if remote.calls != 0 {
		t.Fatal("exchanged code for wrong actor")
	}
	if _, err := g.Complete(ctx, f.owner.ID, state, "code"); err != nil {
		t.Fatal(err)
	}
	c, err := st.GetGitHubConnection(ctx, f.organization.NamespaceID)
	if err != nil || c.Installation.ID != 789 || !c.Enabled {
		t.Fatalf("connection: %+v %v", c, err)
	}
	if _, err = g.Complete(ctx, f.owner.ID, state, "code"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("replay: %v", err)
	}
	if remote.calls != 1 {
		t.Fatal("replayed OAuth code")
	}
}
func TestGitHubCallbackRechecksRevokedManagement(t *testing.T) {
	g, st, f, remote := githubFixture(t)
	ctx := context.Background()
	if err := f.identity.UpdateOrganizationMember(ctx, f.owner.ID, f.organization.ID, f.member.ID, domain.OrganizationAdmin); err != nil {
		t.Fatal(err)
	}
	state := startGitHub(t, g, f.member.ID, f.organization.NamespaceID)
	remote.onAuthorize = func() {
		if err := f.identity.UpdateOrganizationMember(ctx, f.owner.ID, f.organization.ID, f.member.ID, domain.OrganizationMember); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := g.Complete(ctx, f.member.ID, state, "code"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("revoked admin linked: %v", err)
	}
	if _, err := st.GetGitHubConnection(ctx, f.organization.NamespaceID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("connection persisted despite revocation")
	}
}
func TestGitHubCallbackRejectsUnprovenInstallationAndIdentityCollision(t *testing.T) {
	g, st, f, remote := githubFixture(t)
	ctx := context.Background()
	state := startGitHub(t, g, f.owner.ID, f.organization.NamespaceID)
	remote.proof.Installation.ID = 999
	if _, err := g.Complete(ctx, f.owner.ID, state, "code"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("unproven installation: %v", err)
	}
	if err := st.PutGitHubIdentity(ctx, domain.GitHubIdentity{UserID: f.outsider.ID, ExternalID: 123}); err != nil {
		t.Fatal(err)
	}
	state = startGitHub(t, g, f.owner.ID, f.organization.NamespaceID)
	remote.proof.Installation.ID = 789
	if _, err := g.Complete(ctx, f.owner.ID, state, "code"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("identity hijack: %v", err)
	}
}

func (f *githubFake) Installation(context.Context, int64) (domain.GitHubInstallation, error) {
	if f.unavailable {
		return domain.GitHubInstallation{}, domain.ErrNotFound
	}
	return f.proof.Installation, nil
}
func (f *githubFake) Repositories(context.Context, int64) ([]domain.GitHubRepository, error) {
	return append([]domain.GitHubRepository{}, f.repos...), nil
}
func (f *githubFake) Teams(context.Context, int64, string) ([]domain.GitHubTeam, error) {
	return []domain.GitHubTeam{{ID: 99, Name: "Platform", Slug: "platform"}}, nil
}
func (f *githubFake) TeamMembers(context.Context, int64, string, string) ([]int64, error) {
	if f.onMembers != nil {
		f.onMembers()
	}
	return f.members, nil
}
func (f *githubFake) MergedPullRequests(context.Context, int64, domain.GitHubRepository, int) ([]domain.PullRequestMerge, bool, error) {
	return nil, false, nil
}

func runGitHubSyncContract(t *testing.T, g *GitHubConnections, f teamFixture, remote *githubFake) {
	t.Helper()
	ctx := context.Background()
	ns := f.organization.NamespaceID
	start, err := g.Start(ctx, f.owner.ID, ns, remote.proof.Installation.ID)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(start.URL)
	state := u.Query().Get("state")
	if _, err := g.Complete(ctx, f.owner.ID, state, "code"); err != nil {
		t.Fatal(err)
	}
	if err := g.reconcile(ctx, ns); err != nil {
		t.Fatal(err)
	}
	if err := f.identity.SetTeamRepository(ctx, f.owner.ID, f.organization.ID, f.team.ID, f.repository.ID, domain.RolePuller); err != nil {
		t.Fatal(err)
	}
	if err := g.id.withIdentity(ctx, func(tx context.Context) error {
		return g.store.PutGitHubIdentity(tx, domain.GitHubIdentity{UserID: f.member.ID, ExternalID: remote.proof.Identity.ExternalID + 1, Login: "member"})
	}); err != nil {
		t.Fatal(err)
	}
	c, err := g.store.GetGitHubConnection(ctx, ns)
	if err != nil {
		t.Fatal(err)
	}
	remote.members = []int64{remote.proof.Identity.ExternalID + 1, remote.proof.Identity.ExternalID + 2}
	if err = g.Edit(ctx, f.owner.ID, ns, c.Generation, "map-team", domain.GitHubBinding{}, domain.GitHubTeamMapping{TeamID: f.team.ID, ExternalID: 99, SyncMembers: true}); err != nil {
		t.Fatal(err)
	}
	if err = g.reconcile(ctx, ns); err != nil {
		t.Fatal(err)
	}
	role, ok := f.identity.RoleOf(ctx, f.repository.ID, f.member.ID)
	if !ok || role != domain.RolePuller {
		t.Fatalf("derived role missing: %q %v", role, ok)
	}
	members, err := f.identity.ListTeamMembers(ctx, f.owner.ID, f.organization.ID, f.team.ID)
	if err != nil || len(members) != 1 || members[0].Source != "github" {
		t.Fatalf("source-aware membership %+v %v", members, err)
	}
	c, _ = g.store.GetGitHubConnection(ctx, ns)
	if c.UnresolvedMembers != 1 {
		t.Fatal("unverified user was silently imported")
	}
	payload := []byte(fmt.Sprintf(`{"installation":{"id":%d},"action":"removed"}`, remote.proof.Installation.ID))
	if err = g.Receive(ctx, "delivery-"+ns, "membership", payload); err != nil {
		t.Fatal(err)
	}
	if _, ok = f.identity.RoleOf(ctx, f.repository.ID, f.member.ID); ok {
		t.Fatal("revoked derived access still active")
	}
	if err = g.Receive(ctx, "delivery-"+ns, "membership", payload); err != nil {
		t.Fatal(err)
	}
	if err = g.Receive(ctx, "delivery-"+ns, "membership", []byte(`{"installation":{"id":789}}`)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("altered duplicate: %v", err)
	}
	if err = g.reconcile(ctx, ns); err != nil {
		t.Fatal(err)
	}
	if err = f.identity.RemoveOrganizationMember(ctx, f.owner.ID, f.organization.ID, f.member.ID); err != nil {
		t.Fatal(err)
	}
	if err = f.identity.UpdateOrganizationMember(ctx, f.owner.ID, f.organization.ID, f.member.ID, domain.OrganizationMember); err != nil {
		t.Fatal(err)
	}
	if _, ok = f.identity.RoleOf(ctx, f.repository.ID, f.member.ID); ok {
		t.Fatal("rejoining revived an old imported grant")
	}
	c, _ = g.store.GetGitHubConnection(ctx, ns)
	if err = g.Edit(ctx, f.owner.ID, ns, c.Generation, "refresh", domain.GitHubBinding{}, domain.GitHubTeamMapping{}); err != nil {
		t.Fatal(err)
	}
	if err = g.reconcile(ctx, ns); err != nil {
		t.Fatal(err)
	}
	if _, ok = f.identity.RoleOf(ctx, f.repository.ID, f.member.ID); !ok {
		t.Fatal("fresh synchronization did not restore current membership")
	}
	if err = f.identity.UpdateTeamMember(ctx, f.owner.ID, f.organization.ID, f.team.ID, f.member.ID, domain.TeamMaintainer); err != nil {
		t.Fatal(err)
	}
	c, _ = g.store.GetGitHubConnection(ctx, ns)
	if err = g.Edit(ctx, f.owner.ID, ns, c.Generation, "disconnect", domain.GitHubBinding{}, domain.GitHubTeamMapping{}); err != nil {
		t.Fatal(err)
	}
	members, err = f.identity.ListTeamMembers(ctx, f.owner.ID, f.organization.ID, f.team.ID)
	if err != nil || len(members) != 1 || members[0].Role != domain.TeamMaintainer || members[0].Source != "" {
		t.Fatalf("manual membership changed: %+v %v", members, err)
	}
	if _, ok = f.identity.RoleOf(ctx, f.repository.ID, f.member.ID); !ok {
		t.Fatal("disconnect revoked manual grant")
	}
	if err = g.Edit(ctx, f.owner.ID, ns, c.Generation, "refresh", domain.GitHubBinding{}, domain.GitHubTeamMapping{}); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("stale configuration edit accepted")
	}
}
func TestGitHubSyncPreservesManualAccess(t *testing.T) {
	g, _, f, remote := githubFixture(t)
	runGitHubSyncContract(t, g, f, remote)
}
func TestGitHubSyncDoesNotPublishAfterDisconnect(t *testing.T) {
	g, st, f, remote := githubFixture(t)
	ctx := context.Background()
	ns := f.organization.NamespaceID
	state := startGitHub(t, g, f.owner.ID, ns)
	if _, err := g.Complete(ctx, f.owner.ID, state, "code"); err != nil {
		t.Fatal(err)
	}
	if err := g.reconcile(ctx, ns); err != nil {
		t.Fatal(err)
	}
	c, _ := st.GetGitHubConnection(ctx, ns)
	if err := g.Edit(ctx, f.owner.ID, ns, c.Generation, "map-team", domain.GitHubBinding{}, domain.GitHubTeamMapping{TeamID: f.team.ID, ExternalID: 99, SyncMembers: true}); err != nil {
		t.Fatal(err)
	}
	remote.onMembers = func() {
		c, _ := st.GetGitHubConnection(ctx, ns)
		if err := g.Edit(ctx, f.owner.ID, ns, c.Generation, "disconnect", domain.GitHubBinding{}, domain.GitHubTeamMapping{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.reconcile(ctx, ns); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale sync completed: %v", err)
	}
	c, _ = st.GetGitHubConnection(ctx, ns)
	if c.Enabled || c.Status != "disconnected" {
		t.Fatal("worker re-enabled disconnected installation")
	}
}

func TestGitHubBindingUsesImmutableRepositoryAcrossRename(t *testing.T) {
	g, st, f, remote := githubFixture(t)
	ctx := context.Background()
	ns := f.organization.NamespaceID
	remote.repos = []domain.GitHubRepository{{ID: 91, AccountID: 456, FullName: "acme/api"}}
	state := startGitHub(t, g, f.owner.ID, ns)
	if _, err := g.Complete(ctx, f.owner.ID, state, "code"); err != nil {
		t.Fatal(err)
	}
	if err := g.reconcile(ctx, ns); err != nil {
		t.Fatal(err)
	}
	repo := domain.Repo{ID: hh("github-binding"), RepositoryID: f.repository.ID, DefaultBranch: "main", GitRemoteURL: "https://github.com/acme/api.git"}
	if _, err := st.PutRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	c, _ := st.GetGitHubConnection(ctx, ns)
	binding := domain.GitHubBinding{RepositoryID: f.repository.ID, ContextRepoID: repo.ID, ExternalID: 91}
	if err := g.Edit(ctx, f.owner.ID, ns, c.Generation, "bind", binding, domain.GitHubTeamMapping{}); err != nil {
		t.Fatal(err)
	}
	remote.repos[0].FullName = "acme/renamed"
	if err := g.reconcile(ctx, ns); err != nil {
		t.Fatal(err)
	}
	scoped := outbound.WithGitRepository(ctx, repo.ID)
	id, path, err := g.EvidenceInstallation(scoped, "/repos/acme/api/git/commits/abc")
	if err != nil || id != 789 || path != "/repos/acme/renamed/git/commits/abc" {
		t.Fatalf("rename resolution: %d %q %v", id, path, err)
	}
	stored, err := st.GetRepo(ctx, repo.ID)
	if err != nil || stored.GitRemoteURL != repo.GitRemoteURL {
		t.Fatal("rename rewrote immutable Git provenance")
	}
	if _, _, err = g.EvidenceInstallation(scoped, "/repos/another/private/git/commits/abc"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("cross-repository token request: %v", err)
	}
	c, _ = st.GetGitHubConnection(ctx, ns)
	if err = g.Edit(ctx, f.owner.ID, ns, c.Generation, "disconnect", domain.GitHubBinding{}, domain.GitHubTeamMapping{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err = g.EvidenceInstallation(scoped, "/repos/acme/api/git/commits/abc"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("disabled binding fell back to operator token: %v", err)
	}
}

func TestGitHubDeliveryQueuesOnlyBoundRepositoryAndSurvivesWorkerRestart(t *testing.T) {
	g, st, f, remote := githubFixture(t)
	ctx := context.Background()
	ns := f.organization.NamespaceID
	g.core = NewService(st, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(st), st)
	remote.repos = []domain.GitHubRepository{{ID: 91, AccountID: 456, FullName: "acme/api"}}
	state := startGitHub(t, g, f.owner.ID, ns)
	if _, err := g.Complete(ctx, f.owner.ID, state, "code"); err != nil {
		t.Fatal(err)
	}
	if err := g.reconcile(ctx, ns); err != nil {
		t.Fatal(err)
	}
	repo := domain.Repo{ID: hh("github-queue"), RepositoryID: f.repository.ID, DefaultBranch: "main", GitRemoteURL: "https://github.com/acme/api.git"}
	if _, err := st.PutRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	c, _ := st.GetGitHubConnection(ctx, ns)
	if err := g.Edit(ctx, f.owner.ID, ns, c.Generation, "bind", domain.GitHubBinding{RepositoryID: f.repository.ID, ContextRepoID: repo.ID, ExternalID: 91}, domain.GitHubTeamMapping{}); err != nil {
		t.Fatal(err)
	}
	other := repo
	other.ID = hh("unbound-same-origin")
	otherLocal, e := f.identity.CreateOrganizationRepository(ctx, f.owner, f.organization.ID, "other-api")
	if e != nil {
		t.Fatal(e)
	}
	other.RepositoryID = otherLocal.ID
	if _, err := st.PutRepo(ctx, other); err != nil {
		t.Fatal(err)
	}
	body := []byte(fmt.Sprintf(`{"installation":{"id":789},"repository":{"id":91},"action":"closed","number":7,"pull_request":{"merged":true,"merge_commit_sha":%q,"head":{"ref":"feature","sha":%q,"repo":{"id":91}},"base":{"ref":"main","repo":{"id":91}}}}`, strings.Repeat("a", 40), strings.Repeat("b", 40)))
	if err := g.Receive(ctx, "pr-delivery", "pull_request", body); err != nil {
		t.Fatal(err)
	}
	// A new application instance drains durable bytes, without the original request.
	restarted := NewGitHubConnections(f.identity, g.core, st, remote)
	if err := restarted.deliver(ctx, githubHash("pr-delivery")); err != nil {
		t.Fatal(err)
	}
	job, err := st.GetPRJob(ctx, repo.ID, domain.PRPromotionID(repo.ID, 7))
	if err != nil || job.PR.Number != 7 {
		t.Fatalf("PR was not queued: %+v %v", job, err)
	}
	if _, err = st.GetPRJob(ctx, other.ID, domain.PRPromotionID(other.ID, 7)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("installation webhook fanned out by origin instead of binding")
	}
	if err = restarted.Receive(ctx, "pr-delivery", "pull_request", body); err != nil {
		t.Fatal(err)
	}
	if err = restarted.deliver(ctx, githubHash("pr-delivery")); err != nil {
		t.Fatal(err)
	}
	delivered, err := st.GetGitHubDelivery(ctx, githubHash("pr-delivery"))
	if err != nil || !delivered.Done || delivered.Attempts != 1 {
		t.Fatalf("duplicate application: %+v %v", delivered, err)
	}
}

func TestGitHubOrganizationAdminCannotBindOrInspectPrivateContexts(t *testing.T) {
	g, st, f, remote := githubFixture(t)
	ctx := context.Background()
	ns := f.organization.NamespaceID
	if err := f.identity.UpdateOrganizationMember(ctx, f.owner.ID, f.organization.ID, f.member.ID, domain.OrganizationAdmin); err != nil {
		t.Fatal(err)
	}
	remote.repos = []domain.GitHubRepository{{ID: 91, AccountID: 456, FullName: "acme/api"}}
	state := startGitHub(t, g, f.owner.ID, ns)
	if _, err := g.Complete(ctx, f.owner.ID, state, "code"); err != nil {
		t.Fatal(err)
	}
	if err := g.reconcile(ctx, ns); err != nil {
		t.Fatal(err)
	}
	repo := domain.Repo{ID: hh("private-github-binding"), RepositoryID: f.repository.ID, DefaultBranch: "main", GitRemoteURL: "https://github.com/acme/api.git"}
	if _, err := st.PutRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	c, _ := st.GetGitHubConnection(ctx, ns)
	binding := domain.GitHubBinding{RepositoryID: f.repository.ID, ContextRepoID: repo.ID, ExternalID: 91}
	if err := g.Edit(ctx, f.member.ID, ns, c.Generation, "bind", binding, domain.GitHubTeamMapping{}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("org admin bound private context: %v", err)
	}
	if err := g.Edit(ctx, f.owner.ID, ns, c.Generation, "bind", binding, domain.GitHubTeamMapping{}); err != nil {
		t.Fatal(err)
	}
	c, _ = st.GetGitHubConnection(ctx, ns)
	if err := g.Edit(ctx, f.member.ID, ns, c.Generation, "unbind", binding, domain.GitHubTeamMapping{}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("org admin unbound private context: %v", err)
	}
	for _, actor := range []string{f.member.ID, f.owner.ID} {
		view, err := g.Overview(ctx, actor)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, owner := range view.Owners {
			if owner.Namespace.ID != ns {
				continue
			}
			found = true
			want := 0
			if actor == f.owner.ID {
				want = 1
			}
			if len(owner.Repositories) != want || len(owner.ContextRepos) != want || len(owner.Connection.Bindings) != want {
				t.Fatalf("repository visibility for %s: %+v", actor, owner)
			}
		}
		if !found {
			t.Fatal("installation manager cannot see its connection")
		}
	}
}

func TestGitHubEnterpriseVisibilityDoesNotGrantOrganizationManagement(t *testing.T) {
	g, _, f, _ := githubFixture(t)
	ctx := context.Background()
	ent, err := f.identity.CreateEnterprise(ctx, f.owner, "Group", "group-"+domain.NewID("")[:10])
	if err != nil {
		t.Fatal(err)
	}
	if err = f.identity.LinkEnterpriseOrganization(ctx, f.owner.ID, ent.ID, f.organization.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err = g.Enterprise(ctx, f.outsider.ID, ent.ID); err == nil {
		t.Fatal("unrelated user read Enterprise connections")
	}
	if err = f.identity.UpdateEnterpriseMember(ctx, f.owner.ID, ent.ID, f.outsider.ID, domain.EnterpriseOwner); err != nil {
		t.Fatal(err)
	}
	view, err := g.Enterprise(ctx, f.outsider.ID, ent.ID)
	if err != nil || len(view) != 1 || view[0].CanManage {
		t.Fatalf("Enterprise authority crossed Organization boundary: %+v %v", view, err)
	}
	if _, err = g.Start(ctx, f.outsider.ID, f.organization.NamespaceID, 789); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("Enterprise owner started Organization installation: %v", err)
	}
	view, err = g.Enterprise(ctx, f.owner.ID, ent.ID)
	if err != nil || len(view) != 1 || !view[0].CanManage {
		t.Fatalf("Organization owner cannot manage: %+v %v", view, err)
	}
}

type disconnectingGitReader struct {
	*scanReader
	disconnect func()
}

func (r *disconnectingGitReader) ReadCommitTree(ctx context.Context, origin, sha string) (domain.GitTreeEvidence, error) {
	tree, err := r.scanReader.ReadCommitTree(ctx, origin, sha)
	r.disconnect()
	return tree, err
}
func TestGitHubDisconnectDuringEvidenceReadPreventsPublication(t *testing.T) {
	g, st, f, remote := githubFixture(t)
	ctx := systemTestContext()
	ns := f.organization.NamespaceID
	g.core = NewService(st, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(st), st)
	remote.repos = []domain.GitHubRepository{{ID: 91, AccountID: 456, FullName: "acme/api"}}
	state := startGitHub(t, g, f.owner.ID, ns)
	if _, err := g.Complete(ctx, f.owner.ID, state, "code"); err != nil {
		t.Fatal(err)
	}
	if err := g.reconcile(ctx, ns); err != nil {
		t.Fatal(err)
	}
	repo := domain.Repo{ID: hh("revoked-git-read"), RepositoryID: f.repository.ID, DefaultBranch: "main", GitRemoteURL: "https://github.com/acme/api.git"}
	if _, err := st.PutRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	c, _ := st.GetGitHubConnection(ctx, ns)
	if err := g.Edit(ctx, f.owner.ID, ns, c.Generation, "bind", domain.GitHubBinding{RepositoryID: f.repository.ID, ContextRepoID: repo.ID, ExternalID: 91}, domain.GitHubTeamMapping{}); err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("a", 40)
	read := false
	reader := &disconnectingGitReader{scanReader: &scanReader{deltas: map[string]domain.GitCommitDelta{commit: {Commit: commit, Parents: []string{}, Changes: []domain.GitPathChange{}, Complete: true}}}, disconnect: func() {
		read = true
		c, _ := st.GetGitHubConnection(ctx, ns)
		if err := g.Edit(ctx, f.owner.ID, ns, c.Generation, "disconnect", domain.GitHubBinding{}, domain.GitHubTeamMapping{}); err != nil {
			t.Fatal(err)
		}
	}}
	scans, err := NewGitScans(g.core, reader)
	if err != nil {
		t.Fatal(err)
	}
	scans.SetSourceAuthorizer(g)
	if _, err = scans.ObservePush(ctx, repo.GitRemoteURL, "refs/heads/main", "", commit, false, "fixture-push"); err != nil {
		t.Fatal(err)
	}
	if err = scans.Process(ctx, 1); err != nil {
		t.Fatal(err)
	}
	jobs, err := scans.ListScans(ctx, repo.ID, "", 100)
	if err != nil || !read || len(jobs.Items) != 1 || jobs.Items[0].TreeIndexed || jobs.Items[0].Indexed || jobs.Items[0].State == "completed" {
		t.Fatalf("revoked source was published: %+v %v", jobs, err)
	}
}
