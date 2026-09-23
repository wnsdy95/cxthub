package app

import (
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type teamTestStore interface {
	outbound.RepositoryStore
	outbound.OrganizationStore
	outbound.TeamStore
}
type teamFixture struct {
	identity                *IdentityService
	owner, member, outsider domain.User
	organization            domain.Organization
	team                    domain.Team
	repository              domain.Repository
}

func makeTeamFixture(t *testing.T, st teamTestStore) teamFixture {
	t.Helper()
	ctx := systemTestContext()
	suffix := domain.NewID("")[:10]
	f := teamFixture{identity: NewIdentityService(nil, st)}
	for i, p := range []*domain.User{&f.owner, &f.member, &f.outsider} {
		*p = domain.User{ID: domain.NewID("u_"), Username: []string{"owner-", "member-", "outsider-"}[i] + suffix, Email: []string{"owner", "member", "outsider"}[i] + suffix + "@example.test", Name: "Test"}
		if err := st.UpsertUser(ctx, *p); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	f.organization, err = f.identity.CreateOrganization(ctx, f.owner, "Acme", "acme-"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.identity.UpdateOrganizationMember(ctx, f.owner.ID, f.organization.ID, f.member.ID, domain.OrganizationMember); err != nil {
		t.Fatal(err)
	}
	f.repository, err = f.identity.CreateOrganizationRepository(ctx, f.owner, f.organization.ID, "API")
	if err != nil {
		t.Fatal(err)
	}
	f.team, err = f.identity.CreateTeam(ctx, f.owner.ID, f.organization.ID, "Backend", "backend", "")
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func runTeamContract(t *testing.T, st teamTestStore) {
	t.Helper()
	ctx := systemTestContext()
	f := makeTeamFixture(t, st)
	s := f.identity
	baseline := TeamProfile{Name: f.team.Name, Description: f.team.Description}
	next := TeamProfile{Name: "Backend Platform", Description: "Owns the API"}
	if _, err := s.UpdateTeam(ctx, f.member.ID, f.organization.ID, f.team.ID, baseline, next); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("member edited team: %v", err)
	}
	updated, err := s.UpdateTeam(ctx, f.owner.ID, f.organization.ID, f.team.ID, baseline, next)
	if err != nil || updated.Name != next.Name || updated.Description != next.Description || updated.ID != f.team.ID || updated.Slug != f.team.Slug {
		t.Fatalf("team edit: %+v %v", updated, err)
	}
	if _, err := s.UpdateTeam(ctx, f.owner.ID, f.organization.ID, f.team.ID, baseline, TeamProfile{Name: "Stale"}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale edit succeeded: %v", err)
	}
	if _, ok := s.RoleOf(ctx, f.repository.ID, f.member.ID); ok {
		t.Fatal("organization membership granted private access")
	}
	if err := s.SetTeamRepository(ctx, f.member.ID, f.organization.ID, f.team.ID, f.repository.ID, domain.RoleOwner); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("member self-grant: %v", err)
	}
	if err := s.UpdateTeamMember(ctx, f.owner.ID, f.organization.ID, f.team.ID, f.outsider.ID, domain.TeamMember); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("non-organization member joined team: %v", err)
	}
	if err := s.UpdateTeamMember(ctx, f.owner.ID, f.organization.ID, f.team.ID, f.member.ID, domain.TeamMember); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTeamRepository(ctx, f.owner.ID, f.organization.ID, f.team.ID, f.repository.ID, domain.RolePuller); err != nil {
		t.Fatal(err)
	}
	role, ok := s.RoleOf(ctx, f.repository.ID, f.member.ID)
	if !ok || role != domain.RolePuller {
		t.Fatalf("team role: %q %v", role, ok)
	}
	contextService := &Service{repositories: st}
	if other, ok := contextService.repositoryRole(ctx, f.repository.ID, f.member.ID); !ok || other != role {
		t.Fatal("REST/MCP and context service permissions differ")
	}
	list, err := s.ListRepositories(ctx, f.member.ID)
	if err != nil || len(list) != 1 || list[0].ID != f.repository.ID {
		t.Fatalf("team-only repository missing: %+v %v", list, err)
	}
	other, err := s.CreateOrganization(ctx, f.owner, "Other", "other-"+domain.NewID("")[:10])
	if err != nil {
		t.Fatal(err)
	}
	otherTeam, err := s.CreateTeam(ctx, f.owner.ID, other.ID, "Other", "other", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetTeamRepository(ctx, f.owner.ID, other.ID, otherTeam.ID, f.repository.ID, domain.RoleMember); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("cross-org grant: %v", err)
	}
	if err = s.RemoveTeamMember(ctx, f.owner.ID, f.organization.ID, f.team.ID, f.member.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok = s.RoleOf(ctx, f.repository.ID, f.member.ID); ok {
		t.Fatal("removed team member retained access")
	}
	if list, err = s.ListRepositories(ctx, f.member.ID); err != nil || len(list) != 0 {
		t.Fatalf("removed member list: %+v %v", list, err)
	}
	if err = s.UpdateTeamMember(ctx, f.owner.ID, f.organization.ID, f.team.ID, f.member.ID, domain.TeamMember); err != nil {
		t.Fatal(err)
	}
	if err = s.RemoveOrganizationMember(ctx, f.owner.ID, f.organization.ID, f.member.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok = s.RoleOf(ctx, f.repository.ID, f.member.ID); ok {
		t.Fatal("organization removal retained team access")
	}
	if err = s.UpdateOrganizationMember(ctx, f.owner.ID, f.organization.ID, f.member.ID, domain.OrganizationMember); err != nil {
		t.Fatal(err)
	}
	if _, ok = s.RoleOf(ctx, f.repository.ID, f.member.ID); ok {
		t.Fatal("rejoining revived stale team grants")
	}
	if err = st.AddMember(ctx, domain.Membership{RepositoryID: f.repository.ID, UserID: f.member.ID, Role: domain.RoleMember}); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteTeam(ctx, f.owner.ID, f.organization.ID, f.team.ID); err != nil {
		t.Fatal(err)
	}
	if role, ok = s.RoleOf(ctx, f.repository.ID, f.member.ID); !ok || role != domain.RoleMember {
		t.Fatal("deleting team removed direct permission")
	}
}
func TestTeamsUseCurrentMembershipAndRepositoryOwnership(t *testing.T) {
	runTeamContract(t, store.NewFSStore(t.TempDir()))
}
