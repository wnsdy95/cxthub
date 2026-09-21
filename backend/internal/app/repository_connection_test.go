package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestRepositoryConnectionPreservesIdentityAcrossRenameAndTeamAccess(t *testing.T) {
	ctx := context.Background()
	st := store.NewFSStore(t.TempDir())
	f := makeTeamFixture(t, st)
	original := "https://cxthub.example/" + f.organization.Slug + "/" + f.repository.Slug
	id := domain.HashContent([]byte(normalizeGitURL(original)))
	if _, err := st.PutRepo(ctx, domain.Repo{ID: id, RemoteURL: original, RepositoryID: f.repository.ID}); err != nil {
		t.Fatal(err)
	}
	// A new display address must resolve back to the existing content identity.
	name := "renamed"
	if _, err := f.identity.UpdateRepositorySettings(ctx, f.owner.ID, f.repository.ID, RepositoryPatch{Slug: &name}); err != nil {
		t.Fatal(err)
	}
	canonical := "https://cxthub.example/" + f.organization.Slug + "/renamed"
	for _, address := range []string{original, canonical} {
		got, err := f.identity.ResolveRepositoryConnection(ctx, f.owner.ID, address)
		if err != nil {
			t.Fatal(err)
		}
		if got.RepoID != id || got.RemoteURL != original || got.RepositoryID != f.repository.ID || got.CanonicalPath != "/"+f.organization.Slug+"/renamed" {
			t.Fatalf("identity changed: %+v", got)
		}
		if _, err := f.identity.ResolveRepositoryConnection(ctx, "", address); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("private address disclosed to anonymous client: %v", err)
		}
	}
	if err := f.identity.UpdateTeamMember(ctx, f.owner.ID, f.organization.ID, f.team.ID, f.member.ID, domain.TeamMember); err != nil {
		t.Fatal(err)
	}
	if err := f.identity.SetTeamRepository(ctx, f.owner.ID, f.organization.ID, f.team.ID, f.repository.ID, domain.RoleViewer); err != nil {
		t.Fatal(err)
	}
	if _, err := f.identity.ResolveRepositoryConnection(ctx, f.member.ID, canonical); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("viewer obtained pull connection: %v", err)
	}
	if err := f.identity.SetTeamRepository(ctx, f.owner.ID, f.organization.ID, f.team.ID, f.repository.ID, domain.RolePuller); err != nil {
		t.Fatal(err)
	}
	if got, err := f.identity.ResolveRepositoryConnection(ctx, f.member.ID, canonical); err != nil || got.RepoID != id {
		t.Fatalf("team puller connection: %+v %v", got, err)
	}
	if err := f.identity.RemoveTeamMember(ctx, f.owner.ID, f.organization.ID, f.team.ID, f.member.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.identity.ResolveRepositoryConnection(ctx, f.member.ID, canonical); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("revoked team connection: %v", err)
	}
}

func TestRepositoryConnectionKeepsHistoricalHandleWithoutNamespaceRecord(t *testing.T) {
	ctx := context.Background()
	st := store.NewFSStore(t.TempDir())
	user := domain.User{ID: "owner", Username: "alice", Name: "Alice", Email: "alice@example.test"}
	if err := st.UpsertUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	ns := domain.Namespace{ID: domain.NewID("ns_"), Slug: user.Username, UserID: user.ID, Kind: domain.NamespaceUser}
	if err := st.CreateNamespace(ctx, ns); err != nil {
		t.Fatal(err)
	}
	repository := domain.Repository{ID: domain.NewID("ws_"), Name: "Project", Slug: "project", OwnerID: user.ID, OwnerUsername: "old-owner", OwnerNamespaceID: ns.ID}
	if err := st.CreateRepository(ctx, repository); err != nil {
		t.Fatal(err)
	}
	original := "https://cxthub.example/old-owner/project"
	content := domain.Repo{ID: domain.HashContent([]byte(normalizeGitURL(original))), RemoteURL: original, RepositoryID: repository.ID}
	if _, err := st.PutRepo(ctx, content); err != nil {
		t.Fatal(err)
	}
	repository.OwnerUsername = user.Username
	if err := st.CreateRepository(ctx, repository); err != nil {
		t.Fatal(err)
	}
	identity := NewIdentityService(nil, st)
	got, err := identity.ResolveRepositoryConnection(ctx, user.ID, original)
	if err != nil || got.RepoID != content.ID {
		t.Fatalf("historical handle: %+v %v", got, err)
	}
	service := &Service{repositories: st}
	bound, err := service.repositoryForURL(ctx, original)
	if err != nil || bound != repository.ID {
		t.Fatalf("push binding for historical handle: %s %v", bound, err)
	}
	if _, err = identity.ReadableRepository(ctx, "old-owner", "project", "outsider"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("alias leaked private context: %v", err)
	}
}
