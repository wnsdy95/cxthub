package app

import (
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestNamespaceAdministration(t *testing.T) {
	runNamespaceAdministration(t, store.NewFSStore(t.TempDir()))
}
func runNamespaceAdministration(t *testing.T, st invitationTestStore) {
	ctx := systemTestContext()
	f := makeTeamFixture(t, st)
	s := f.identity
	original := f.organization.Slug
	if _, err := s.RenameCollaborationSpace(ctx, f.member.ID, "organization", f.organization.ID, original, "new-name"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("member renamed: %v", err)
	}
	next := "renamed-" + domain.NewID("")[:10]
	if _, err := s.RenameCollaborationSpace(ctx, f.owner.ID, "organization", f.organization.ID, original, next); err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{original, next} {
		org, err := st.GetOrganizationBySlug(ctx, slug)
		if err != nil || org.ID != f.organization.ID || org.Slug != next {
			t.Fatalf("org alias %s: %+v %v", slug, org, err)
		}
		repo, err := st.GetRepositoryByPath(ctx, slug, f.repository.Slug)
		if err != nil || repo.ID != f.repository.ID || repo.OwnerUsername != next {
			t.Fatalf("repository alias %s: %+v %v", slug, repo, err)
		}
	}
	if _, err := s.CreateOrganization(ctx, f.owner, "Claim old name", original); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("namespace hijack: %v", err)
	}
	if _, err := s.RenameCollaborationSpace(ctx, f.owner.ID, "organization", f.organization.ID, original, "stale"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale rename: %v", err)
	}
	e, err := s.CreateEnterprise(ctx, f.owner, "Group", "group-"+domain.NewID("")[:10])
	if err != nil {
		t.Fatal(err)
	}
	renamed := e.Slug + "-new"
	if _, err = s.RenameCollaborationSpace(ctx, f.owner.ID, "enterprise", e.ID, e.Slug, renamed); err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{e.Slug, renamed} {
		got, err := st.GetEnterpriseBySlug(ctx, slug)
		if err != nil || got.ID != e.ID || got.Slug != renamed {
			t.Fatalf("enterprise alias: %+v %v", got, err)
		}
	}
	if _, err = s.CreateEnterprise(ctx, f.owner, "Claim old", e.Slug); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("enterprise hijack: %v", err)
	}
	// Move the same repository into another organization, then back. Team access
	// from the source organization must not revive when the repository returns.
	destination, err := s.CreateOrganization(ctx, f.owner, "Destination", "dest-"+domain.NewID("")[:10])
	if err != nil {
		t.Fatal(err)
	}
	if err = s.UpdateTeamMember(ctx, f.owner.ID, f.organization.ID, f.team.ID, f.member.ID, domain.TeamMember); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTeamRepository(ctx, f.owner.ID, f.organization.ID, f.team.ID, f.repository.ID, domain.RoleMember); err != nil {
		t.Fatal(err)
	}
	if _, err = s.TransferRepositoryNamespace(ctx, f.member.ID, f.repository.ID, f.repository.OwnerNamespaceID, f.repository.Slug, destination.Slug); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("member transferred: %v", err)
	}
	moved, err := s.TransferRepositoryNamespace(ctx, f.owner.ID, f.repository.ID, f.repository.OwnerNamespaceID, f.repository.Slug, destination.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if moved.ID != f.repository.ID || moved.OwnerNamespaceID != destination.NamespaceID {
		t.Fatalf("changed identity: %+v", moved)
	}
	if _, ok := s.RoleOf(ctx, moved.ID, f.member.ID); ok {
		t.Fatal("source team traveled")
	}
	old, err := st.GetRepositoryByPath(ctx, original, moved.Slug)
	if err != nil || old.ID != moved.ID || old.OwnerNamespaceID != destination.NamespaceID {
		t.Fatalf("original alias lost after transfer: %+v %v", old, err)
	}
	restored, err := s.TransferRepositoryNamespace(ctx, f.owner.ID, moved.ID, moved.OwnerNamespaceID, moved.Slug, next)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.RoleOf(ctx, restored.ID, f.member.ID); ok {
		t.Fatal("old team grant revived")
	}
	// An inherited organization owner must not gain a permanent direct owner
	// grant merely by moving someone else's repository between organizations.
	for _, org := range []domain.Organization{f.organization, destination} {
		if err = s.UpdateOrganizationMember(ctx, f.owner.ID, org.ID, f.member.ID, domain.OrganizationOwner); err != nil {
			t.Fatal(err)
		}
	}
	moved, err = s.TransferRepositoryNamespace(ctx, f.member.ID, restored.ID, restored.OwnerNamespaceID, restored.Slug, destination.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if moved.OwnerID != restored.OwnerID {
		t.Fatal("transfer silently replaced the repository's recorded creator")
	}
	if err = s.UpdateOrganizationMember(ctx, f.owner.ID, destination.ID, f.member.ID, domain.OrganizationMember); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.RoleOf(ctx, moved.ID, f.member.ID); ok {
		t.Fatal("transfer converted inherited ownership into permanent direct access")
	}
	// Personal transfer requires the current namespace owner; preserves direct members.
	personal, err := s.CreateRepository(ctx, f.owner, "Personal")
	if err != nil {
		t.Fatal(err)
	}
	if err = st.AddMember(ctx, domain.Membership{RepositoryID: personal.ID, UserID: f.outsider.ID, Role: domain.RolePuller}); err != nil {
		t.Fatal(err)
	}
	moved, err = s.TransferRepositoryNamespace(ctx, f.owner.ID, personal.ID, personal.OwnerNamespaceID, personal.Slug, destination.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if role, ok := s.RoleOf(ctx, moved.ID, f.outsider.ID); !ok || role != domain.RolePuller {
		t.Fatal("direct access lost")
	}
	if _, err = s.TransferRepositoryNamespace(ctx, f.owner.ID, moved.ID, personal.OwnerNamespaceID, personal.Slug, next); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale transfer: %v", err)
	}
	_, err = s.TransferRepositoryNamespace(ctx, f.owner.ID, moved.ID, moved.OwnerNamespaceID, moved.Slug, f.owner.Username)
	if err != nil {
		t.Fatal(err)
	}
}
