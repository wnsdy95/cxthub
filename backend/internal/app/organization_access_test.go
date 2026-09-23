package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// Run against both adapters: an owner promoted after repository creation must
// have identical access to one present at creation, without copied memberships.
func runOrganizationOwnerAccess(t *testing.T, st teamTestStore) {
	t.Helper()
	ctx := context.Background()
	f := makeTeamFixture(t, st)
	s := f.identity
	check := func(want domain.MemberRole) {
		t.Helper()
		role, ok := s.RoleOf(ctx, f.repository.ID, f.member.ID)
		if role != want || ok != (want != "") {
			t.Fatalf("role=%q ok=%v want=%q", role, ok, want)
		}
		repos, err := s.ListRepositories(ctx, f.member.ID)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, repo := range repos {
			found = found || repo.ID == f.repository.ID
		}
		if found != (want != "") {
			t.Fatalf("list disagrees with role: %+v", repos)
		}
		if other, _ := (&Service{repositories: st}).repositoryRole(ctx, f.repository.ID, f.member.ID); other != role {
			t.Fatal("context command and identity role disagree")
		}
	}
	for _, role := range []domain.OrganizationRole{domain.OrganizationAdmin, domain.OrganizationMember, domain.OrganizationOwner} {
		if err := s.UpdateOrganizationMember(ctx, f.owner.ID, f.organization.ID, f.member.ID, role); err != nil {
			t.Fatal(err)
		}
		if role == domain.OrganizationOwner {
			check(domain.RoleOwner)
		} else {
			check("")
		}
	}
	if direct, err := st.IsMember(ctx, f.repository.ID, f.member.ID); err != nil || direct {
		t.Fatal("owner inheritance materialized direct membership")
	}
	created, err := s.CreateOrganizationRepository(ctx, f.owner, f.organization.ID, "Later")
	if err != nil {
		t.Fatal(err)
	}
	if role, ok := s.RoleOf(ctx, created.ID, f.member.ID); !ok || role != domain.RoleOwner {
		t.Fatal("future repository missing owner authority")
	}
	if _, err = s.ReadableRepository(ctx, f.organization.Slug, f.repository.Slug, f.member.ID); err != nil {
		t.Fatal(err)
	}
	policy := "owner"
	if _, err = s.UpdateRepositorySettings(ctx, f.member.ID, f.repository.ID, RepositoryPatch{SecretsPolicy: &policy}); err != nil {
		t.Fatal(err)
	}
	if !s.CanTransferOwnership(ctx, f.repository.ID, f.member.ID) {
		t.Fatal("owner cannot transfer organization anchor")
	}
	personal, err := s.CreateRepository(ctx, f.owner, "Personal")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateOrganization(ctx, f.owner, "Other", "other-"+domain.NewID("")[:10])
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := s.CreateOrganizationRepository(ctx, f.owner, other.ID, "Foreign")
	if err != nil {
		t.Fatal(err)
	}
	for _, repo := range []domain.Repository{personal, foreign} {
		if _, ok := s.RoleOf(ctx, repo.ID, f.member.ID); ok || s.CanTransferOwnership(ctx, repo.ID, f.member.ID) {
			t.Fatal("owner authority crossed namespace")
		}
	}
	if err = st.AddMember(ctx, domain.Membership{RepositoryID: f.repository.ID, UserID: f.member.ID, Role: domain.RoleViewer}); err != nil {
		t.Fatal(err)
	}
	check(domain.RoleOwner)
	if err = s.UpdateOrganizationMember(ctx, f.owner.ID, f.organization.ID, f.member.ID, domain.OrganizationAdmin); err != nil {
		t.Fatal(err)
	}
	check(domain.RoleViewer)
	if s.CanTransferOwnership(ctx, f.repository.ID, f.member.ID) {
		t.Fatal("demoted owner retained transfer")
	}
	if _, err = s.UpdateRepositorySettings(ctx, f.member.ID, f.repository.ID, RepositoryPatch{SecretsPolicy: &policy}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("demoted owner changed settings: %v", err)
	}
	if err = st.RemoveMember(ctx, f.repository.ID, f.member.ID); err != nil {
		t.Fatal(err)
	}
	check("")
	if err = s.UpdateTeamMember(ctx, f.owner.ID, f.organization.ID, f.team.ID, f.member.ID, domain.TeamMember); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTeamRepository(ctx, f.owner.ID, f.organization.ID, f.team.ID, f.repository.ID, domain.RolePuller); err != nil {
		t.Fatal(err)
	}
	check(domain.RolePuller)
	if err = s.UpdateOrganizationMember(ctx, f.owner.ID, f.organization.ID, f.member.ID, domain.OrganizationOwner); err != nil {
		t.Fatal(err)
	}
	check(domain.RoleOwner)
	if err = s.RemoveOrganizationMember(ctx, f.owner.ID, f.organization.ID, f.member.ID); err != nil {
		t.Fatal(err)
	}
	check("")
}

func TestOrganizationOwnerAccessContract(t *testing.T) {
	runOrganizationOwnerAccess(t, store.NewFSStore(t.TempDir()))
}

func runOrganizationOwnerCommands(t *testing.T, st interface {
	teamTestStore
	outbound.MetadataStore
	outbound.BlobStore
}) {
	t.Helper()
	ctx := context.Background()
	f := makeTeamFixture(t, st)
	if err := f.identity.UpdateOrganizationMember(ctx, f.owner.ID, f.organization.ID, f.member.ID, domain.OrganizationOwner); err != nil {
		t.Fatal(err)
	}
	repo := domain.HashContent([]byte(f.repository.ID))
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: f.repository.ID}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(st, st, nil, nil, st)
	in := inbound.SaveSecretsInput{RepoID: repo, ActorID: f.member.ID, Envelope: secretsTestEnvelope("aaaaaaaaaaaa"), Edit: domain.SecretsEdit{ExpectedRevision: "absent"}}
	saved, err := svc.SaveSecrets(ctx, in)
	if err != nil {
		t.Fatalf("owner secrets: %v", err)
	}
	if err = svc.authorizeJoin(ctx, repo, f.member.ID, false); err != nil {
		t.Fatalf("owner join: %v", err)
	}
	f.repository.Archived = true
	if err = st.CreateRepository(ctx, f.repository); err != nil {
		t.Fatal(err)
	}
	in.Edit.ExpectedRevision = saved.Revision
	if _, err = svc.SaveSecrets(ctx, in); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("archived owner secrets: %v", err)
	}
	if err = svc.authorizeJoin(ctx, repo, f.member.ID, false); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("archived owner join: %v", err)
	}
	f.repository.Archived = false
	if err = st.CreateRepository(ctx, f.repository); err != nil {
		t.Fatal(err)
	}
	if err = f.identity.UpdateOrganizationMember(ctx, f.owner.ID, f.organization.ID, f.member.ID, domain.OrganizationMember); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.SaveSecrets(ctx, in); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("demoted owner secrets: %v", err)
	}
	if err = svc.authorizeJoin(ctx, repo, f.member.ID, false); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("demoted owner join: %v", err)
	}
	// These commands must also honor current team grants through the same policy.
	if err = f.identity.UpdateTeamMember(ctx, f.owner.ID, f.organization.ID, f.team.ID, f.member.ID, domain.TeamMember); err != nil {
		t.Fatal(err)
	}
	if err = f.identity.SetTeamRepository(ctx, f.owner.ID, f.organization.ID, f.team.ID, f.repository.ID, domain.RoleMaintainer); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.SaveSecrets(ctx, in); err != nil {
		t.Fatalf("team secrets: %v", err)
	}
	if err = svc.authorizeJoin(ctx, repo, f.member.ID, false); err != nil {
		t.Fatalf("team join: %v", err)
	}
}

func TestOrganizationOwnerCommands(t *testing.T) {
	runOrganizationOwnerCommands(t, store.NewFSStore(t.TempDir()))
}

func TestOrganizationOwnerTransferKeepsNamespace(t *testing.T) {
	st := store.NewFSStore(t.TempDir())
	f := makeTeamFixture(t, st)
	ctx := context.Background()
	if err := f.identity.UpdateOrganizationMember(ctx, f.owner.ID, f.organization.ID, f.member.ID, domain.OrganizationOwner); err != nil {
		t.Fatal(err)
	}
	if err := st.AddMember(ctx, domain.Membership{RepositoryID: f.repository.ID, UserID: f.outsider.ID, Role: domain.RoleMember}); err != nil {
		t.Fatal(err)
	}
	got, err := f.identity.TransferOwnership(ctx, f.member.ID, f.repository.ID, f.outsider.ID)
	if err != nil || got.OwnerID != f.outsider.ID || got.OwnerNamespaceID != f.repository.OwnerNamespaceID || got.OwnerUsername != f.repository.OwnerUsername {
		t.Fatalf("organization transfer: %+v %v", got, err)
	}
}
