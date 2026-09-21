package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func runOrganizationOffboard(t *testing.T, st teamTestStore) {
	t.Helper()
	ctx := context.Background()
	for _, access := range []string{"revoke", "retain"} {
		f := makeTeamFixture(t, st)
		s := f.identity
		if err := st.AddMember(ctx, domain.Membership{RepositoryID: f.repository.ID, UserID: f.member.ID, Role: domain.RolePuller}); err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateTeamMember(ctx, f.owner.ID, f.organization.ID, f.team.ID, f.member.ID, domain.TeamMember); err != nil {
			t.Fatal(err)
		}
		if err := s.SetTeamRepository(ctx, f.owner.ID, f.organization.ID, f.team.ID, f.repository.ID, domain.RoleMember); err != nil {
			t.Fatal(err)
		}
		if err := s.OffboardOrganizationMember(ctx, f.owner.ID, f.organization.ID, f.member.ID, ""); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("missing choice: %v", err)
		}
		if err := s.OffboardOrganizationMember(ctx, f.owner.ID, f.organization.ID, f.member.ID, access); err != nil {
			t.Fatal(err)
		}
		role, ok := s.RoleOf(ctx, f.repository.ID, f.member.ID)
		if access == "revoke" && ok || access == "retain" && (!ok || role != domain.RolePuller) {
			t.Fatalf("%s: retained role %s %v", access, role, ok)
		}
		if err := s.UpdateOrganizationMember(ctx, f.owner.ID, f.organization.ID, f.member.ID, domain.OrganizationMember); err != nil {
			t.Fatal(err)
		}
		if next, _ := s.RoleOf(ctx, f.repository.ID, f.member.ID); next != role {
			t.Fatal("rejoining restored old team access")
		}
		// The human ownership anchor must be handed over before removing all access.
		if err := s.UpdateOrganizationMember(ctx, f.owner.ID, f.organization.ID, f.outsider.ID, domain.OrganizationOwner); err != nil {
			t.Fatal(err)
		}
		if err := s.OffboardOrganizationMember(ctx, f.outsider.ID, f.organization.ID, f.owner.ID, "revoke"); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("owner offboarding: %v", err)
		}
		if role, ok := s.OrganizationRoleOf(ctx, f.organization.ID, f.owner.ID); !ok || role != domain.OrganizationOwner {
			t.Fatal("failed owner offboarding changed membership")
		}
	}
}

func TestOrganizationOffboardingRequiresExplicitAccessChoice(t *testing.T) {
	runOrganizationOffboard(t, store.NewFSStore(t.TempDir()))
}
