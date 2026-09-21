package domain

import "testing"

func TestEffectiveRepositoryRoleRequiresCurrentOrganizationAndTeamMembership(t *testing.T) {
	repository := Repository{ID: "repository", OwnerID: "owner", OwnerNamespaceID: "organization-namespace"}
	grant := TeamRepositoryAccess{TeamID: "backend", RepositoryID: repository.ID, OrganizationNamespaceID: repository.OwnerNamespaceID, UserID: "reader", OrganizationMember: true, TeamMember: true, Role: RoleMember}
	if role, ok := EffectiveRepositoryRole(repository, nil, "reader", []TeamRepositoryAccess{grant}); !ok || role != RoleMember {
		t.Fatal("valid team grant denied")
	}
	for _, change := range []func(*TeamRepositoryAccess){
		func(g *TeamRepositoryAccess) { g.OrganizationMember = false },
		func(g *TeamRepositoryAccess) { g.TeamMember = false },
		func(g *TeamRepositoryAccess) { g.OrganizationNamespaceID = "other-organization" },
		func(g *TeamRepositoryAccess) { g.RepositoryID = "other-repository" },
		func(g *TeamRepositoryAccess) { g.UserID = "other-user" },
		func(g *TeamRepositoryAccess) { g.Role = "corrupt-role" },
	} {
		bad := grant
		change(&bad)
		if _, ok := EffectiveRepositoryRole(repository, nil, "reader", []TeamRepositoryAccess{bad}); ok {
			t.Fatal("stale or foreign team grant authorized access")
		}
	}
	direct := []Membership{{RepositoryID: repository.ID, UserID: "reader", Role: RoleMaintainer}}
	if role, ok := EffectiveRepositoryRole(repository, direct, "reader", []TeamRepositoryAccess{grant}); !ok || role != RoleMaintainer {
		t.Fatal("team grant reduced an explicit direct grant")
	}
	if _, ok := EffectiveRepositoryRole(repository, nil, "", []TeamRepositoryAccess{grant}); ok {
		t.Fatal("team authorized anonymous access")
	}
}
