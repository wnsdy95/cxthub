package domain

import "testing"

func TestOrganizationOwnerAuthorityRequiresMatchingCurrentFacts(t *testing.T) {
	repository := Repository{ID: "repo", OwnerID: "creator", OwnerNamespaceID: "org"}
	access := OrganizationRepositoryAccess{RepositoryID: "repo", OrganizationNamespaceID: "org", UserID: "actor", Role: OrganizationOwner}
	direct := []Membership{{RepositoryID: "repo", UserID: "actor", Role: RoleViewer}}
	if role, ok := EffectiveRepositoryRole(repository, direct, "actor", nil, access); !ok || role != RoleOwner {
		t.Fatal("owner authority did not supersede lower direct grant")
	}
	if !CanTransferRepository(repository, "actor", access) {
		t.Fatal("organization owner cannot transfer anchor")
	}
	for _, change := range []func(*OrganizationRepositoryAccess){
		func(a *OrganizationRepositoryAccess) { a.UserID = "other" },
		func(a *OrganizationRepositoryAccess) { a.RepositoryID = "other" },
		func(a *OrganizationRepositoryAccess) { a.OrganizationNamespaceID = "other" },
		func(a *OrganizationRepositoryAccess) { a.OrganizationNamespaceID = "" },
		func(a *OrganizationRepositoryAccess) { a.Role = OrganizationAdmin },
		func(a *OrganizationRepositoryAccess) { a.Role = OrganizationMember },
		func(a *OrganizationRepositoryAccess) { a.Role = "" },
	} {
		bad := access
		change(&bad)
		if role, ok := EffectiveRepositoryRole(repository, direct, "actor", nil, bad); !ok || role != RoleViewer {
			t.Fatal("invalid owner facts overrode direct grant")
		}
		if CanTransferRepository(repository, "actor", bad) {
			t.Fatal("invalid owner facts authorized transfer")
		}
	}
	if _, ok := EffectiveRepositoryRole(repository, nil, "", nil, access); ok {
		t.Fatal("anonymous owner inheritance")
	}
}
