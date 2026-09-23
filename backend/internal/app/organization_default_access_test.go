package app

import (
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func runOrganizationDefaultAccess(t *testing.T, st teamTestStore) {
	t.Helper()
	ctx := systemTestContext()
	f := makeTeamFixture(t, st)
	s := f.identity
	policy, err := s.GetOrganizationPolicy(ctx, f.owner.ID, f.organization.ID)
	if err != nil {
		t.Fatal(err)
	}
	check := func(want domain.MemberRole) {
		t.Helper()
		role, ok := s.RoleOf(ctx, f.repository.ID, f.member.ID)
		if role != want || ok != (want != "") {
			t.Fatalf("role=%s ok=%v want=%s", role, ok, want)
		}
		repos, err := s.ListRepositories(ctx, f.member.ID)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, r := range repos {
			found = found || r.ID == f.repository.ID
		}
		if found != ok {
			t.Fatalf("list/authorization disagreement: %+v", repos)
		}
	}
	check("")
	policy.DefaultRepositoryRole = domain.RolePuller
	policy, err = s.UpdateOrganizationPolicy(ctx, f.owner.ID, policy)
	if err != nil {
		t.Fatal(err)
	}
	check(domain.RolePuller)
	if role, _ := s.RoleOf(ctx, f.repository.ID, f.owner.ID); role != domain.RoleOwner {
		t.Fatal("base role reduced owner authority")
	}
	if _, ok := s.RoleOf(ctx, f.repository.ID, f.outsider.ID); ok {
		t.Fatal("base permission leaked to outsider")
	}
	if err := st.AddMember(ctx, domain.Membership{RepositoryID: f.repository.ID, UserID: f.member.ID, Role: domain.RoleMember}); err != nil {
		t.Fatal(err)
	}
	check(domain.RoleMember)
	old := policy
	policy.DefaultRepositoryRole = ""
	policy, err = s.UpdateOrganizationPolicy(ctx, f.owner.ID, policy)
	if err != nil {
		t.Fatal(err)
	}
	check(domain.RoleMember)
	if _, err := s.UpdateOrganizationPolicy(ctx, f.owner.ID, old); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale policy overwrite: %v", err)
	}
	if err := s.UpdateOrganizationMember(ctx, f.owner.ID, f.organization.ID, f.member.ID, domain.OrganizationAdmin); err != nil {
		t.Fatal(err)
	}
	policy.DefaultRepositoryRole = domain.RoleOwner
	if _, err := s.UpdateOrganizationPolicy(ctx, f.member.ID, policy); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("admin changed base permission: %v", err)
	}
	if err := st.RemoveMember(ctx, f.repository.ID, f.member.ID); err != nil {
		t.Fatal(err)
	}
	check("")
	policy.DefaultRepositoryRole = domain.RoleViewer
	policy, err = s.UpdateOrganizationPolicy(ctx, f.owner.ID, policy)
	if err != nil {
		t.Fatal(err)
	}
	created, err := s.CreateOrganizationRepository(ctx, f.owner, f.organization.ID, "Future")
	if err != nil {
		t.Fatal(err)
	}
	if role, _ := s.RoleOf(ctx, created.ID, f.member.ID); role != domain.RoleViewer {
		t.Fatal("new repository did not inherit baseline")
	}
	if err := s.RemoveOrganizationMember(ctx, f.owner.ID, f.organization.ID, f.member.ID); err != nil {
		t.Fatal(err)
	}
	check("")
}

func TestOrganizationDefaultAccess(t *testing.T) {
	runOrganizationDefaultAccess(t, store.NewFSStore(t.TempDir()))
}
